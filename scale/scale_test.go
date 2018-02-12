package scale

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"flag"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/ethereum/go-ethereum/whisper/shhclient"
	"github.com/ethereum/go-ethereum/whisper/whisperv5"
	"github.com/status-im/status-go/scale/project"
	"github.com/stretchr/testify/suite"
)

const (
	defaultRetries  = 10
	defaultInterval = 500 * time.Millisecond
)

var (
	keep          = flag.Bool("keep", false, "keep the cluster after tests are finished.")
	wnodeScale    = flag.Int("wnode-scale", 12, "size of the whisper cluster.")
	dockerTimeout = flag.Duration("docker-timeout", 5*time.Second, "Docker cluster startup timeout.")
)

type Whisp struct {
	Name    string
	RPC     string
	Metrics string
}

func MakeWhisps(containers []types.Container) []Whisp {
	whisps := []Whisp{}
	for _, container := range containers {
		w := Whisp{Name: container.Names[0]}
		for _, port := range container.Ports {
			if port.PrivatePort == 8080 {
				w.Metrics = fmt.Sprintf("http://%s:%d/metrics", port.IP, port.PublicPort)
			} else if port.PrivatePort == 8545 {
				w.RPC = fmt.Sprintf("http://%s:%d", port.IP, port.PublicPort)
			}
		}
		whisps = append(whisps, w)
	}
	return whisps
}

func TestWhisperScale(t *testing.T) {
	suite.Run(t, new(WhisperScaleSuite))
}

type WhisperScaleSuite struct {
	suite.Suite

	p      project.Project
	whisps []Whisp
}

func (w *WhisperScaleSuite) SetupSuite() {
	flag.Parse()
}

func (s *WhisperScaleSuite) SetupTest() {
	cli, err := client.NewEnvClient()
	s.Require().NoError(err)
	cwd, err := os.Getwd()
	s.Require().NoError(err)
	s.p = project.New(filepath.Join(filepath.Dir(cwd), "_assets", "compose", "wnode-test-cluster"), cli)
	s.NoError(s.p.Up(project.UpOpts{
		Scale: map[string]int{"wnode": *wnodeScale},
		Wait:  *dockerTimeout,
	}))
	containers, err := s.p.Containers(project.FilterOpts{SvcName: "wnode"})
	s.Require().NoError(err)
	s.whisps = MakeWhisps(containers)
}

func (s *WhisperScaleSuite) TearDownTest() {
	if !*keep {
		s.NoError(s.p.Down()) // make it optional and wait
	}
}

func (s *WhisperScaleSuite) NoErrors(errors []error) {
	var unexpectedErr bool
	for i, err := range errors {
		if err != nil {
			unexpectedErr = true
		}
		s.NoError(err, "whisp", s.whisps[i], "received unexpected error")
	}
	if unexpectedErr {
		s.Require().FailNow("no errors expected")
	}

}

func runConcurrent(whisps []Whisp, f func(i int, w Whisp) error) []error {
	var wg sync.WaitGroup
	errs := make([]error, len(whisps))
	for i, w := range whisps {
		wg.Add(1)
		go func(i int, w Whisp) {
			defer wg.Done()
			if err := f(i, w); err != nil {
				errs[i] = err
			}
		}(i, w)
	}
	wg.Wait()
	return errs
}

func runWithRetries(retries int, interval time.Duration, f func() error) error {
	for {
		if err := f(); err != nil {
			retries--
			if retries == 0 {
				return err
			}
			time.Sleep(interval)
			continue
		}
		return nil
	}
}

// TestSymKeyMessaging generates 100 messages with 0.5s interval between them
// messages are generated by 5 peers
// After all messages are generated we will wait till old new and duplicates
// will be delivered to all peers. After that we will count how many new and old
// envelopes received by each peer.
func (s *WhisperScaleSuite) TestSymKeyMessaging() {
	msgNum := 100
	interval := 500 * time.Millisecond
	whispCount := 9
	if len(s.whisps) < whispCount {
		whispCount = len(s.whisps)
	}
	s.NoErrors(runConcurrent(s.whisps[:whispCount], func(i int, w Whisp) error {
		c, err := shhclient.Dial(w.RPC)
		if err != nil {
			return err
		}
		var info whisperv5.Info
		if err := runWithRetries(defaultRetries, defaultInterval, func() (err error) {
			info, err = c.Info(context.TODO())
			return err
		}); err != nil {
			return err
		}

		symkey, err := c.NewSymmetricKey(context.TODO())
		if err != nil {
			return err
		}
		for j := 0; j < msgNum; j++ {
			// we should only fail if there was burst of errors
			if err := c.Post(context.TODO(), whisperv5.NewMessage{
				SymKeyID:  symkey,
				PowTarget: info.MinPow,
				PowTime:   200,
				Topic:     whisperv5.TopicType{0x03, 0x02, 0x02, 0x05},
				Payload:   []byte("hello"),
			}); err != nil {
				return err
			}
			time.Sleep(interval)
		}
		return nil
	}))

	var mu sync.Mutex
	reports := make(Summary, len(s.whisps))
	s.NoErrors(runConcurrent(s.whisps, func(i int, w Whisp) error {
		var prevOldCount, prevNewCount float64
		for {
			// wait till no duplicates are received
			// given that transmission cycle is 200 ms, 5s should be enough
			var oldCount, newCount float64
			if err := runWithRetries(defaultRetries, defaultInterval, func() (err error) {
				oldCount, newCount, err = getOldNewEnvelopesCount(w.Metrics)
				return err
			}); err != nil {
				return err
			}
			prevNewCount = newCount
			if oldCount > prevOldCount {
				prevOldCount = oldCount
				time.Sleep(5 * time.Second)
				continue
			}
			break
		}
		mu.Lock()
		reports[i].NewEnvelopes = prevNewCount
		reports[i].OldEnvelopes = prevOldCount
		mu.Unlock()
		return nil
	}))
	for i, report := range reports {
		// senders messages are not counted
		if i < whispCount {
			s.Equal(float64(msgNum*(whispCount-1)), report.NewEnvelopes)
		} else {
			s.Equal(float64(msgNum*whispCount), report.NewEnvelopes)
		}
	}

	s.NoErrors(runConcurrent(s.whisps, func(i int, w Whisp) error {
		metrics, err := ethMetrics(w.RPC)
		if err != nil {
			return err
		}
		mu.Lock()
		reports[i].Ingress = metrics.Peer2Peer.InboundTraffic.Overall
		reports[i].Egress = metrics.Peer2Peer.OutboundTraffic.Overall
		mu.Unlock()
		return nil
	}))
	s.True(reports.MeanOldPerNew() < 1.5)
	reports.Print(os.Stdout)
}
