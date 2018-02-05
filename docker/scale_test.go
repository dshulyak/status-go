package scale

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"flag"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/ethereum/go-ethereum/whisper/shhclient"
	"github.com/ethereum/go-ethereum/whisper/whisperv5"
	"github.com/status-im/status-go/docker/project"
	"github.com/stretchr/testify/suite"
)

var keep = flag.Bool("keep", false, "keep the cluster after tests are finished.")

type Whisp struct {
	Name    string
	Rpc     string
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
				w.Rpc = fmt.Sprintf("http://%s:%d", port.IP, port.PublicPort)
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
	s.NoError(err)
	s.p = project.New("wnode-test-cluster", cli)
	s.NoError(s.p.Up(project.UpOpts{
		Scale: map[string]int{"wnode": 12},
		Wait:  true,
	}))
	containers, err := s.p.Containers(project.FilterOpts{SvcName: "wnode"})
	s.NoError(err)
	s.whisps = MakeWhisps(containers)
}

func (s *WhisperScaleSuite) TearDownTest() {
	if !*keep {
		s.NoError(s.p.Down()) // make it optional and wait
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
	whispCount := 5
	var wg sync.WaitGroup
	if len(s.whisps) < whispCount {
		whispCount = len(s.whisps)
	}
	wg.Add(whispCount)
	for i := 0; i < whispCount; i++ {
		w := s.whisps[i]
		c, err := shhclient.Dial(w.Rpc)
		s.NoError(err)
		for {
			// wait till whisper is ready
			_, err := c.Info(context.TODO())
			if err != nil {
				time.Sleep(500 * time.Millisecond)
				continue
			}
			break
		}
		go func(c *shhclient.Client) {
			defer wg.Done()
			symkey, err := c.NewSymmetricKey(context.TODO())
			s.NoError(err)
			info, err := c.Info(context.TODO())
			s.NoError(err)
			for j := 0; j < msgNum; j++ {
				s.NoError(c.Post(context.TODO(), whisperv5.NewMessage{
					SymKeyID:  symkey,
					PowTarget: info.MinPow,
					PowTime:   200,
					Topic:     whisperv5.TopicType{0x03, 0x02, 0x02, 0x05},
					Payload:   []byte("hello"),
				}))
				time.Sleep(interval)
			}
		}(c)
	}
	wg.Wait()
	// wait for all duplicates to be delivered across the network
	var (
		mu                        sync.Mutex
		sumNew, sumOld, oldPerNew float64
	)
	for i, w := range s.whisps {
		wg.Add(1)
		go func(i int, w Whisp) {
			defer wg.Done()
			var prevOld, prevNew float64
			for {
				// wait till no duplicates are received
				// given that transmission cycle is 200 ms, 5s should be enough
				whispOld, whispNew, err := getOldNewEnvelopesCount(w.Metrics)
				s.NoError(err)
				if err != nil {
					return
				}
				prevNew = whispNew
				if whispOld > prevOld {
					prevOld = whispOld
					time.Sleep(5 * time.Second)
					continue
				}
				break
			}
			if i < whispCount {
				s.Equal(float64(msgNum*(whispCount-1)), prevNew)
			} else {
				s.Equal(float64(msgNum*whispCount), prevNew)
			}
			mu.Lock()
			sumNew += prevNew
			sumOld += prevOld
			whispOldPerNew := (prevOld / prevNew)
			oldPerNew += whispOldPerNew
			mu.Unlock()
			os.Stdout.Write([]byte(fmt.Sprintln("=== REPORT:", w.Name, prevOld, prevNew, whispOldPerNew)))
		}(i, w)
	}
	wg.Wait()
	var ingress, egress float64
	for _, w := range s.whisps {
		wg.Add(1)
		go func(w Whisp) {
			defer wg.Done()
			metrics, err := ethMetrics(w.Rpc)
			s.NoError(err)
			mu.Lock()
			ingress += metrics.Peer2Peer.InboundTraffic.Overall
			egress += metrics.Peer2Peer.OutboundTraffic.Overall
			mu.Unlock()
		}(w)
	}
	wg.Wait()
	s.True(oldPerNew/float64(len(s.whisps)) < 3.4)
	os.Stdout.Write([]byte(fmt.Sprintln("=== SUMMARY",
		"\ningress:", ingress/1024/1024, "mb",
		"\negress:", egress/1024/1024, "mb",
		"\nduplicates: ", sumOld,
		"\nnew: ", sumNew,
		"\nmean old per new for each peer: ", oldPerNew/float64(len(s.whisps)))))
}
