package queue

import (
	"context"
	"errors"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/keystore"
	gethcommon "github.com/ethereum/go-ethereum/common"

	"github.com/status-im/status-go/geth/common"
	"github.com/stretchr/testify/suite"
)

func TestQueueTestSuite(t *testing.T) {
	suite.Run(t, new(QueueTestSuite))
}

type QueueTestSuite struct {
	suite.Suite
	queue *TxQueue
}

func (s *QueueTestSuite) SetupTest() {
	s.queue = NewQueue()
	s.queue.Start()
}

func (s *QueueTestSuite) TearDownTest() {
	s.queue.Stop()
}

func (s *QueueTestSuite) TestGetTransaction() {
	tx := common.CreateTransaction(context.Background(), common.SendTxArgs{})
	s.queue.Enqueue(tx)
	enquedTx, err := s.queue.Get(tx.ID)
	s.NoError(err)
	s.Equal(tx, enquedTx)

	// verify that tx was marked as being inprogress
	_, err = s.queue.Get(tx.ID)
	s.Equal(ErrQueuedTxInProgress, err)
}

func (s *QueueTestSuite) testDone(hash gethcommon.Hash, err error) (*common.QueuedTx, <-chan common.QueuedTxResult) {
	tx := common.CreateTransaction(context.Background(), common.SendTxArgs{})
	c := s.queue.Enqueue(tx)
	s.NoError(s.queue.Done(tx.ID, hash, err))
	return tx, c
}

func (s *QueueTestSuite) TestDoneSuccess() {
	hash := gethcommon.Hash{1}
	tx, c := s.testDone(hash, nil)
	// event is sent only if transaction was removed from a queue
	select {
	case rst := <-c:
		s.NoError(rst.Err)
		s.Equal(hash, rst.Hash)
		s.False(s.queue.Has(tx.ID))
	default:
		s.Fail("No event was sent to Done channel")
	}
}

func (s *QueueTestSuite) TestDoneTransientError() {
	hash := gethcommon.Hash{1}
	err := keystore.ErrDecrypt
	tx, _ := s.testDone(hash, err)
	s.True(s.queue.Has(tx.ID))
	_, inp := s.queue.inprogress[tx.ID]
	s.False(inp)
}

func (s *QueueTestSuite) TestDoneError() {
	hash := gethcommon.Hash{1}
	err := errors.New("test")
	tx, c := s.testDone(hash, err)
	// event is sent only if transaction was removed from a queue
	select {
	case rst := <-c:
		s.Equal(err, rst.Err)
		s.NotEqual(hash, rst.Hash)
		s.Equal(gethcommon.Hash{}, rst.Hash)
		s.False(s.queue.Has(tx.ID))
	default:
		s.Fail("No event was sent to Done channel")
	}
}

func (s QueueTestSuite) TestMultipleDone() {
	hash := gethcommon.Hash{1}
	err := keystore.ErrDecrypt
	tx, _ := s.testDone(hash, err)
	s.NoError(s.queue.Done(tx.ID, hash, nil))
	s.Equal(ErrQueuedTxIDNotFound, s.queue.Done(tx.ID, hash, errors.New("timeout")))
}

func (s *QueueTestSuite) TestEviction() {
	var first *common.QueuedTx
	for i := 0; i < DefaultTxQueueCap; i++ {
		tx := common.CreateTransaction(context.Background(), common.SendTxArgs{})
		if first == nil {
			first = tx
		}
		s.queue.Enqueue(tx)
	}
	s.Equal(DefaultTxQueueCap, s.queue.Count())
	tx := common.CreateTransaction(context.Background(), common.SendTxArgs{})
	s.queue.Enqueue(tx)
	s.Equal(DefaultTxQueueCap, s.queue.Count())
	s.False(s.queue.Has(first.ID))
}
