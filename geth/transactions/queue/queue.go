package queue

import (
	"errors"
	"fmt"
	"sync"

	"github.com/ethereum/go-ethereum/accounts/keystore"
	gethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/status-im/status-go/geth/account"
	"github.com/status-im/status-go/geth/common"
	"github.com/status-im/status-go/geth/log"
)

const (
	// DefaultTxQueueCap defines how many items can be queued.
	DefaultTxQueueCap = int(35)
)

var (
	//ErrQueuedTxIDNotFound - error transaction hash not found
	ErrQueuedTxIDNotFound = errors.New("transaction hash not found")
	//ErrQueuedTxTimedOut - error transaction sending timed out
	ErrQueuedTxTimedOut = errors.New("transaction sending timed out")
	//ErrQueuedTxDiscarded - error transaction discarded
	ErrQueuedTxDiscarded = errors.New("transaction has been discarded")
	//ErrQueuedTxInProgress - error transaction in progress
	ErrQueuedTxInProgress = errors.New("transaction is in progress")
	//ErrQueuedTxAlreadyProcessed - error transaction has already processed
	ErrQueuedTxAlreadyProcessed = errors.New("transaction has been already processed")
	//ErrInvalidCompleteTxSender - error transaction with invalid sender
	ErrInvalidCompleteTxSender = errors.New("transaction can only be completed by the same account which created it")
	ErrTxEvicted               = errors.New("transactions was evicted")
)

// remove from queue on any error (except for transient ones) and propagate
var transientErrs = map[string]bool{
	keystore.ErrDecrypt.Error():          true, // wrong password
	ErrInvalidCompleteTxSender.Error():   true, // completing tx create from another account
	account.ErrNoAccountSelected.Error(): true, // account not selected
}

type empty struct{}

// TxQueue is capped container that holds pending transactions
type TxQueue struct {
	mu            sync.RWMutex // to guard transactions map
	transactions  map[common.QueuedTxID]*common.QueuedTx
	lth           int
	inprogress    map[common.QueuedTxID]empty
	subscriptions map[common.QueuedTxID]chan common.QueuedTxResult

	evictables []common.QueuedTxID

	// when this channel is closed, all queue channels processing must cease (incoming queue, processing queued items etc)
	stopped chan struct{}
}

// NewTransactionQueue make new transaction queue
func NewQueue() *TxQueue {
	log.Info("initializing transaction queue")
	return &TxQueue{
		transactions:  make(map[common.QueuedTxID]*common.QueuedTx),
		inprogress:    make(map[common.QueuedTxID]empty),
		subscriptions: make(map[common.QueuedTxID]chan common.QueuedTxResult),
		evictables:    make([]common.QueuedTxID, 0, DefaultTxQueueCap),
	}
}

// Start starts enqueue and eviction loops
func (q *TxQueue) Start() {
	log.Info("starting transaction queue")

	if q.stopped != nil {
		return
	}

	q.stopped = make(chan struct{})
}

// Stop stops transaction enqueue and eviction loops
func (q *TxQueue) Stop() {
	log.Info("stopping transaction queue")

	if q.stopped == nil {
		return
	}

	close(q.stopped) // stops all processing loops (enqueue, eviction etc)
	q.stopped = nil

	log.Info("finally stopped transaction queue")
}

// Reset is to be used in tests only, as it simply creates new transaction map, w/o any cleanup of the previous one
func (q *TxQueue) Reset() {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.transactions = make(map[common.QueuedTxID]*common.QueuedTx)
	q.inprogress = make(map[common.QueuedTxID]empty)
}

// Enqueue enqueues incoming transaction
func (q *TxQueue) Enqueue(tx *common.QueuedTx) <-chan common.QueuedTxResult {
	log.Info(fmt.Sprintf("enqueue transaction: %s", tx.ID))
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.lth == DefaultTxQueueCap {
		// time for making room for new txs
		q.evict()
	}
	q.lth++
	q.evictables = append(q.evictables, tx.ID)
	q.transactions[tx.ID] = tx
	c := make(chan common.QueuedTxResult, 1)
	q.subscriptions[tx.ID] = c
	// notify handler
	log.Info("calling txEnqueueHandler")
	return c
}

func (q *TxQueue) evict() {
	var (
		i    int
		txID common.QueuedTxID
	)
	for i, txID = range q.evictables {
		// if tx is not in store - we already processed it
		// just remove
		if _, ok := q.transactions[txID]; !ok {
			break
		}
		// we never drop inprogress tx
		if _, ok := q.inprogress[txID]; !ok {
			break
		}
	}
	if i == DefaultTxQueueCap {
		// queue is full, all tx are in progress
		// will return an error here
		return
	}
	// do not drop tx silently
	// consider to split it to evict soft and evict hard
	// evict soft never drops unprocessed tx and usually executed
	// when any tx is done
	q.remove(txID)
	copy(q.evictables[i:], q.evictables[i+1:])
	q.evictables[len(q.evictables)-1] = ""
	q.evictables = q.evictables[:len(q.evictables)-1]
}

// Get returns transaction by transaction identifier
func (q *TxQueue) Get(id common.QueuedTxID) (*common.QueuedTx, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if tx, ok := q.transactions[id]; ok {
		if _, inprogress := q.inprogress[id]; inprogress {
			return tx, ErrQueuedTxInProgress
		}
		q.inprogress[id] = empty{}
		return tx, nil
	}
	return nil, ErrQueuedTxIDNotFound
}

// Remove removes transaction by transaction identifier
func (q *TxQueue) Remove(id common.QueuedTxID) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.remove(id)
}

func (q *TxQueue) remove(id common.QueuedTxID) {
	delete(q.transactions, id)
	delete(q.inprogress, id)
	delete(q.subscriptions, id)
	q.lth--
}

// Done removes transaction from queue if no error or error is not transient
// and notify subscribers
func (q *TxQueue) Done(id common.QueuedTxID, hash gethcommon.Hash, err error) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if tx, ok := q.transactions[id]; !ok {
		return ErrQueuedTxIDNotFound
	} else {
		q.done(tx.ID, hash, err)
	}
	return nil
}

func (q *TxQueue) done(txID common.QueuedTxID, hash gethcommon.Hash, err error) {
	delete(q.inprogress, txID)
	// hash is updated only if err is nil
	if err == nil {
		q.subscriptions[txID] <- common.QueuedTxResult{Hash: hash, Err: err}
		q.remove(txID)
		return
	}
	_, transient := transientErrs[err.Error()]
	if !transient {
		q.subscriptions[txID] <- common.QueuedTxResult{Err: err}
		q.remove(txID)
	}
}

// Count returns number of currently queued transactions
func (q *TxQueue) Count() int {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return len(q.transactions)
}

// Has checks whether transaction with a given identifier exists in queue
func (q *TxQueue) Has(id common.QueuedTxID) bool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	_, ok := q.transactions[id]
	return ok
}
