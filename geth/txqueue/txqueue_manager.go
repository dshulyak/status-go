package txqueue

import (
	"context"
	"math/big"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	gethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/pborman/uuid"
	"github.com/status-im/status-go/geth/common"
	"github.com/status-im/status-go/geth/log"
	"github.com/status-im/status-go/geth/signal"
)

const (
	// EventTransactionQueued is triggered when send transaction request is queued
	EventTransactionQueued = "transaction.queued"

	// EventTransactionFailed is triggered when send transaction request fails
	EventTransactionFailed = "transaction.failed"

	// SendTxDefaultErrorCode is sent by default, when error is not nil, but type is unknown/unexpected.
	SendTxDefaultErrorCode = SendTransactionDefaultErrorCode

	defaultGas = 90000

	cancelTimeout = time.Minute
)

// Send transaction response codes
const (
	SendTransactionNoErrorCode        = "0"
	SendTransactionDefaultErrorCode   = "1"
	SendTransactionPasswordErrorCode  = "2"
	SendTransactionTimeoutErrorCode   = "3"
	SendTransactionDiscardedErrorCode = "4"
)

var txReturnCodes = map[error]string{ // deliberately strings, in case more meaningful codes are to be returned
	nil:                  SendTransactionNoErrorCode,
	keystore.ErrDecrypt:  SendTransactionPasswordErrorCode,
	ErrQueuedTxTimedOut:  SendTransactionTimeoutErrorCode,
	ErrQueuedTxDiscarded: SendTransactionDiscardedErrorCode,
}

// Manager provides means to manage internal Status Backend (injected into LES)
type Manager struct {
	nodeManager    common.NodeManager
	accountManager common.AccountManager
	txQueue        *TxQueue
	ethTxClient    EthereumTransactor
}

// NewManager returns a new Manager.
func NewManager(nodeManager common.NodeManager, accountManager common.AccountManager) *Manager {
	return &Manager{
		nodeManager:    nodeManager,
		accountManager: accountManager,
		txQueue:        NewTransactionQueue(),
	}
}

// Start starts accepting new transactions into the queue.
func (m *Manager) Start() {
	log.Info("start Manager")
	m.ethTxClient = NewEthTxClient(m.nodeManager.RPCClient())
	m.txQueue.Start()
}

// Stop stops accepting new transactions into the queue.
func (m *Manager) Stop() {
	log.Info("stop Manager")
	m.txQueue.Stop()
}

// TransactionQueue returns a reference to the queue.
func (m *Manager) TransactionQueue() common.TxQueue {
	return m.txQueue
}

// CreateTransaction returns a transaction object.
func (m *Manager) CreateTransaction(ctx context.Context, args common.SendTxArgs) *common.QueuedTx {
	return &common.QueuedTx{
		ID:      common.QueuedTxID(uuid.New()),
		Hash:    gethcommon.Hash{},
		Context: ctx,
		Args:    args,
		Done:    make(chan struct{}, 1),
		Discard: make(chan struct{}, 1),
	}
}

// QueueTransaction puts a transaction into the queue.
func (m *Manager) QueueTransaction(tx *common.QueuedTx) error {
	to := "<nil>"
	if tx.Args.To != nil {
		to = tx.Args.To.Hex()
	}
	log.Info("queue a new transaction", "id", tx.ID, "from", tx.Args.From.Hex(), "to", to)

	return m.txQueue.Enqueue(tx)
}

// WaitForTransaction adds a transaction to the queue and blocks
// until it's completed, discarded or times out.
func (m *Manager) WaitForTransaction(tx *common.QueuedTx) error {
	log.Info("wait for transaction", "id", tx.ID)

	// now wait up until transaction is:
	// - completed (via CompleteQueuedTransaction),
	// - discarded (via DiscardQueuedTransaction)
	// - or times out
	select {
	case <-tx.Done:
		m.NotifyOnQueuedTxReturn(tx, tx.Err)
		return tx.Err
	case <-tx.Discard:
		m.NotifyOnQueuedTxReturn(tx, ErrQueuedTxDiscarded)
		return ErrQueuedTxDiscarded
	case <-time.After(DefaultTxSendCompletionTimeout * time.Second):
		m.NotifyOnQueuedTxReturn(tx, ErrQueuedTxTimedOut)
		return ErrQueuedTxTimedOut
	}
}

// NotifyOnQueuedTxReturn calls a handler when a transaction resolves.
func (m *Manager) NotifyOnQueuedTxReturn(queuedTx *common.QueuedTx, err error) {
	m.txQueue.NotifyOnQueuedTxReturn(queuedTx, err)
}

// CompleteTransaction instructs backend to complete sending of a given transaction.
// TODO(adam): investigate a possible bug that calling this method multiple times with the same Transaction ID
// results in sending multiple transactions.
func (m *Manager) CompleteTransaction(id common.QueuedTxID, password string) (gethcommon.Hash, error) {
	log.Info("complete transaction", "id", id)

	queuedTx, err := m.txQueue.Get(id)
	if err != nil {
		log.Warn("could not get a queued transaction", "err", err)
		return gethcommon.Hash{}, err
	}

	err = m.txQueue.StartProcessing(queuedTx)
	if err != nil {
		return gethcommon.Hash{}, err
	}
	defer m.txQueue.StopProcessing(queuedTx)

	selectedAccount, err := m.accountManager.SelectedAccount()
	if err != nil {
		log.Warn("failed to get a selected account", "err", err)
		return gethcommon.Hash{}, err
	}

	// make sure that only account which created the tx can complete it
	if queuedTx.Args.From.Hex() != selectedAccount.Address.Hex() {
		log.Warn("queued transaction does not belong to the selected account", "err", ErrInvalidCompleteTxSender)
		m.NotifyOnQueuedTxReturn(queuedTx, ErrInvalidCompleteTxSender)
		return gethcommon.Hash{}, ErrInvalidCompleteTxSender
	}
	// Send the transaction finally.
	hash, err := m.completeTransaction(selectedAccount, queuedTx, password)

	// when incorrect sender tries to complete the account,
	// notify and keep tx in queue (so that correct sender can complete)
	if err == keystore.ErrDecrypt {
		log.Warn("failed to complete transaction", "err", err)
		m.NotifyOnQueuedTxReturn(queuedTx, err)
		return hash, err
	}

	log.Info("finally completed transaction", "id", queuedTx.ID, "hash", hash, "err", err)

	queuedTx.Hash = hash
	queuedTx.Err = err
	queuedTx.Done <- struct{}{}

	return hash, err
}

func (m *Manager) completeTransaction(selectedAccount *common.SelectedExtKey, queuedTx *common.QueuedTx, password string) (gethcommon.Hash, error) {
	log.Info("complete transaction", "id", queuedTx.ID)
	var emptyHash gethcommon.Hash
	log.Info("verifying account password for transaction", "id", queuedTx.ID)
	config, err := m.nodeManager.NodeConfig()
	if err != nil {
		return emptyHash, err
	}
	_, err = m.accountManager.VerifyAccountPassword(config.KeyStoreDir, selectedAccount.Address.String(), password)
	if err != nil {
		log.Warn("failed to verify account", "account", selectedAccount.Address.String(), "error", err.Error())
		return emptyHash, err
	}

	// update transaction with nonce, gas price and gas estimates
	ctx, cancel := context.WithTimeout(context.Background(), cancelTimeout)
	defer cancel()
	nonce, err := m.ethTxClient.PendingNonceAt(ctx, queuedTx.Args.From)
	if err != nil {
		return emptyHash, err
	}
	args := queuedTx.Args
	var gasPrice *big.Int
	if args.GasPrice == nil {
		ctx, cancel = context.WithTimeout(context.Background(), cancelTimeout)
		defer cancel()
		gasPrice, err = m.ethTxClient.SuggestGasPrice(ctx)
		if err != nil {
			return emptyHash, err
		}

	} else {
		gasPrice = (*big.Int)(args.GasPrice)
	}

	chainID := big.NewInt(int64(config.NetworkID))
	data := []byte(args.Data)
	value := (*big.Int)(args.Value)
	toAddr := gethcommon.Address{}
	if args.To != nil {
		toAddr = *args.To
	}
	ctx, cancel = context.WithTimeout(context.Background(), cancelTimeout)
	defer cancel()
	gas, err := m.ethTxClient.EstimateGas(ctx, ethereum.CallMsg{
		From:     args.From,
		To:       args.To,
		GasPrice: gasPrice,
		Value:    value,
		Data:     data,
	})
	if err != nil {
		return emptyHash, err
	}
	if gas.Cmp(big.NewInt(defaultGas)) == -1 {
		log.Info("default gas will be used. estimated gas", gas, "is lower than", defaultGas)
		gas = big.NewInt(defaultGas)
	}

	log.Info(
		"preparing raw transaction",
		"from", args.From.Hex(),
		"to", toAddr.Hex(),
		"gas", gas,
		"gasPrice", gasPrice,
		"value", value,
	)
	tx := types.NewTransaction(nonce, toAddr, value, gas, gasPrice, data)
	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(chainID), selectedAccount.AccountKey.PrivateKey)
	if err != nil {
		return emptyHash, err
	}
	ctx, cancel = context.WithTimeout(context.Background(), cancelTimeout)
	defer cancel()
	if err := m.ethTxClient.SendTransaction(ctx, signedTx); err != nil {
		return emptyHash, err
	}
	return signedTx.Hash(), nil
}

// CompleteTransactions instructs backend to complete sending of multiple transactions
func (m *Manager) CompleteTransactions(ids []common.QueuedTxID, password string) map[common.QueuedTxID]common.RawCompleteTransactionResult {
	results := make(map[common.QueuedTxID]common.RawCompleteTransactionResult)

	for _, txID := range ids {
		txHash, txErr := m.CompleteTransaction(txID, password)
		results[txID] = common.RawCompleteTransactionResult{
			Hash:  txHash,
			Error: txErr,
		}
	}

	return results
}

// DiscardTransaction discards a given transaction from transaction queue
func (m *Manager) DiscardTransaction(id common.QueuedTxID) error {
	queuedTx, err := m.txQueue.Get(id)
	if err != nil {
		return err
	}

	// remove from queue, before notifying SendTransaction
	m.txQueue.Remove(queuedTx.ID)

	// allow SendTransaction to return
	queuedTx.Err = ErrQueuedTxDiscarded
	queuedTx.Discard <- struct{}{} // sendTransaction() waits on this, notify so that it can return

	return nil
}

// DiscardTransactions discards given multiple transactions from transaction queue
func (m *Manager) DiscardTransactions(ids []common.QueuedTxID) map[common.QueuedTxID]common.RawDiscardTransactionResult {
	results := make(map[common.QueuedTxID]common.RawDiscardTransactionResult)

	for _, txID := range ids {
		err := m.DiscardTransaction(txID)
		if err != nil {
			results[txID] = common.RawDiscardTransactionResult{
				Error: err,
			}
		}
	}

	return results
}

// SendTransactionEvent is a signal sent on a send transaction request
type SendTransactionEvent struct {
	ID        string            `json:"id"`
	Args      common.SendTxArgs `json:"args"`
	MessageID string            `json:"message_id"`
}

// TransactionQueueHandler returns handler that processes incoming tx queue requests
func (m *Manager) TransactionQueueHandler() func(queuedTx *common.QueuedTx) {
	return func(queuedTx *common.QueuedTx) {
		log.Info("calling TransactionQueueHandler")
		signal.Send(signal.Envelope{
			Type: EventTransactionQueued,
			Event: SendTransactionEvent{
				ID:        string(queuedTx.ID),
				Args:      queuedTx.Args,
				MessageID: common.MessageIDFromContext(queuedTx.Context),
			},
		})
	}
}

// SetTransactionQueueHandler sets a handler that will be called
// when a new transaction is enqueued.
func (m *Manager) SetTransactionQueueHandler(fn common.EnqueuedTxHandler) {
	m.txQueue.SetEnqueueHandler(fn)
}

// ReturnSendTransactionEvent is a JSON returned whenever transaction send is returned
type ReturnSendTransactionEvent struct {
	ID           string            `json:"id"`
	Args         common.SendTxArgs `json:"args"`
	MessageID    string            `json:"message_id"`
	ErrorMessage string            `json:"error_message"`
	ErrorCode    string            `json:"error_code"`
}

// TransactionReturnHandler returns handler that processes responses from internal tx manager
func (m *Manager) TransactionReturnHandler() func(queuedTx *common.QueuedTx, err error) {
	return func(queuedTx *common.QueuedTx, err error) {
		if err == nil {
			return
		}

		// discard notifications with empty tx
		if queuedTx == nil {
			return
		}

		// error occurred, signal up to application
		signal.Send(signal.Envelope{
			Type: EventTransactionFailed,
			Event: ReturnSendTransactionEvent{
				ID:           string(queuedTx.ID),
				Args:         queuedTx.Args,
				MessageID:    common.MessageIDFromContext(queuedTx.Context),
				ErrorMessage: err.Error(),
				ErrorCode:    m.sendTransactionErrorCode(err),
			},
		})
	}
}

func (m *Manager) sendTransactionErrorCode(err error) string {
	if code, ok := txReturnCodes[err]; ok {
		return code
	}

	return SendTxDefaultErrorCode
}

// SetTransactionReturnHandler sets a handler that will be called
// when a transaction is about to return or when a recoverable error occurred.
// Recoverable error is, for instance, wrong password.
func (m *Manager) SetTransactionReturnHandler(fn common.EnqueuedTxReturnHandler) {
	m.txQueue.SetTxReturnHandler(fn)
}

// SendTransactionRPCHandler is a handler for eth_sendTransaction method.
// It accepts one param which is a slice with a map of transaction params.
func (m *Manager) SendTransactionRPCHandler(ctx context.Context, args ...interface{}) (interface{}, error) {
	log.Info("SendTransactionRPCHandler called")

	// TODO(adam): it's a hack to parse arguments as common.RPCCall can do that.
	// We should refactor parsing these params to a separate struct.
	rpcCall := common.RPCCall{Params: args}

	tx := m.CreateTransaction(ctx, rpcCall.ToSendTxArgs())

	if err := m.QueueTransaction(tx); err != nil {
		return nil, err
	}

	if err := m.WaitForTransaction(tx); err != nil {
		return nil, err
	}

	return tx.Hash.Hex(), nil
}
