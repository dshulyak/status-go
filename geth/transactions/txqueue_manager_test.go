package transactions

import (
	"context"
	"math/big"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/keystore"
	gethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	gethrpc "github.com/ethereum/go-ethereum/rpc"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/suite"

	"github.com/status-im/status-go/geth/common"
	"github.com/status-im/status-go/geth/params"
	"github.com/status-im/status-go/geth/rpc"
	"github.com/status-im/status-go/geth/transactions/fake"
	"github.com/status-im/status-go/geth/transactions/queue"
	. "github.com/status-im/status-go/testing"
)

func TestTxQueueTestSuite(t *testing.T) {
	suite.Run(t, new(TxQueueTestSuite))
}

type TxQueueTestSuite struct {
	suite.Suite
	nodeManagerMockCtrl    *gomock.Controller
	nodeManagerMock        *common.MockNodeManager
	accountManagerMockCtrl *gomock.Controller
	accountManagerMock     *common.MockAccountManager
	server                 *gethrpc.Server
	client                 *gethrpc.Client
	txServiceMockCtrl      *gomock.Controller
	txServiceMock          *fake.MockFakePublicTransactionPoolAPI
}

func (s *TxQueueTestSuite) SetupTest() {
	s.nodeManagerMockCtrl = gomock.NewController(s.T())
	s.accountManagerMockCtrl = gomock.NewController(s.T())
	s.txServiceMockCtrl = gomock.NewController(s.T())

	s.nodeManagerMock = common.NewMockNodeManager(s.nodeManagerMockCtrl)
	s.accountManagerMock = common.NewMockAccountManager(s.accountManagerMockCtrl)

	s.server, s.txServiceMock = fake.NewTestServer(s.txServiceMockCtrl)
	s.client = gethrpc.DialInProc(s.server)
	rpclient, _ := rpc.NewClient(s.client, params.UpstreamRPCConfig{})
	s.nodeManagerMock.EXPECT().RPCClient().Return(rpclient)
}

func (s *TxQueueTestSuite) TearDownTest() {
	s.nodeManagerMockCtrl.Finish()
	s.accountManagerMockCtrl.Finish()
	s.txServiceMockCtrl.Finish()
	s.server.Stop()
	s.client.Close()
}

func (s *TxQueueTestSuite) setupTransactionPoolAPI(account *common.SelectedExtKey, nonce hexutil.Uint64, gas hexutil.Big, txErr error) {
	s.txServiceMock.EXPECT().GetTransactionCount(gomock.Any(), account.Address, gethrpc.PendingBlockNumber).Return(&nonce, nil)
	s.txServiceMock.EXPECT().GasPrice(gomock.Any()).Return(big.NewInt(10), nil)
	s.txServiceMock.EXPECT().EstimateGas(gomock.Any(), gomock.Any()).Return(&gas, nil)
	s.txServiceMock.EXPECT().SendRawTransaction(gomock.Any(), gomock.Any()).Return(gethcommon.Hash{}, txErr)
}

func (s *TxQueueTestSuite) setupStatusBackend(account *common.SelectedExtKey, password string) {
	nodeConfig, nodeErr := params.NewNodeConfig("/tmp", params.RopstenNetworkID, true)
	s.nodeManagerMock.EXPECT().NodeConfig().Return(nodeConfig, nodeErr)
	s.accountManagerMock.EXPECT().SelectedAccount().Return(account, nil)
	s.accountManagerMock.EXPECT().VerifyAccountPassword(nodeConfig.KeyStoreDir, account.Address.String(), password).Return(
		nil, nil)
}

func (s *TxQueueTestSuite) TestCompleteTransaction() {
	password := TestConfig.Account1.Password
	key, _ := crypto.GenerateKey()
	account := &common.SelectedExtKey{
		Address:    common.FromAddress(TestConfig.Account1.Address),
		AccountKey: &keystore.Key{PrivateKey: key},
	}
	s.setupStatusBackend(account, password)

	nonce := hexutil.Uint64(10)
	gas := hexutil.Big(*big.NewInt(defaultGas + 1))
	s.setupTransactionPoolAPI(account, nonce, gas, nil)

	txQueueManager := NewManager(s.nodeManagerMock, s.accountManagerMock)

	txQueueManager.Start()
	defer txQueueManager.Stop()

	tx := common.CreateTransaction(context.Background(), common.SendTxArgs{
		From: common.FromAddress(TestConfig.Account1.Address),
		To:   common.ToAddress(TestConfig.Account2.Address),
	})

	c := txQueueManager.QueueTransaction(tx)

	w := make(chan struct{})
	go func() {
		_, errCompleteTransaction := txQueueManager.CompleteTransaction(tx.ID, password)
		s.NoError(errCompleteTransaction)
		close(w)
	}()

	rst := txQueueManager.WaitForTransaction(tx, c)
	// Check that error is assigned to the transaction.
	s.NoError(rst.Err)
	// Transaction should be already removed from the queue.
	s.False(txQueueManager.TransactionQueue().Has(tx.ID))
	<-w
}

func (s *TxQueueTestSuite) TestCompleteTransactionMultipleTimes() {
	password := TestConfig.Account1.Password
	key, _ := crypto.GenerateKey()
	account := &common.SelectedExtKey{
		Address:    common.FromAddress(TestConfig.Account1.Address),
		AccountKey: &keystore.Key{PrivateKey: key},
	}
	s.setupStatusBackend(account, password)

	nonce := hexutil.Uint64(10)
	gas := hexutil.Big(*big.NewInt(defaultGas + 1))
	s.setupTransactionPoolAPI(account, nonce, gas, nil)

	txQueueManager := NewManager(s.nodeManagerMock, s.accountManagerMock)
	txQueueManager.DisableNotificactions()

	txQueueManager.Start()
	defer txQueueManager.Stop()

	tx := common.CreateTransaction(context.Background(), common.SendTxArgs{
		From: common.FromAddress(TestConfig.Account1.Address),
		To:   common.ToAddress(TestConfig.Account2.Address),
	})

	c := txQueueManager.QueueTransaction(tx)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var completedTx int
	var inprogressTx int
	txCount := 3
	for i := 0; i < txCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := txQueueManager.CompleteTransaction(tx.ID, password)
			mu.Lock()
			if err == nil {
				completedTx++
			} else if err == queue.ErrQueuedTxInProgress {
				inprogressTx++
			} else {
				s.Fail("tx failed with unexpected error: ", err.Error())
			}
			mu.Unlock()
		}()
	}

	rst := txQueueManager.WaitForTransaction(tx, c)
	// Check that error is assigned to the transaction.
	s.NoError(rst.Err)
	// Transaction should be already removed from the queue.
	s.False(txQueueManager.TransactionQueue().Has(tx.ID))

	// Wait for all CompleteTransaction calls.
	wg.Wait()
	s.Equal(1, completedTx, "only 1 tx expected to be completed")
	s.Equal(txCount-1, inprogressTx, txCount-1, "txs expected to be reported as inprogress")
}

func (s *TxQueueTestSuite) TestAccountMismatch() {
	s.accountManagerMock.EXPECT().SelectedAccount().Return(&common.SelectedExtKey{
		Address: common.FromAddress(TestConfig.Account2.Address),
	}, nil)

	txQueueManager := NewManager(s.nodeManagerMock, s.accountManagerMock)
	txQueueManager.DisableNotificactions()

	txQueueManager.Start()
	defer txQueueManager.Stop()

	tx := common.CreateTransaction(context.Background(), common.SendTxArgs{
		From: common.FromAddress(TestConfig.Account1.Address),
		To:   common.ToAddress(TestConfig.Account2.Address),
	})

	txQueueManager.QueueTransaction(tx)

	_, err := txQueueManager.CompleteTransaction(tx.ID, TestConfig.Account1.Password)
	s.Equal(err, queue.ErrInvalidCompleteTxSender)

	// Transaction should stay in the queue as mismatched accounts
	// is a recoverable error.
	s.True(txQueueManager.TransactionQueue().Has(tx.ID))
}

func (s *TxQueueTestSuite) TestInvalidPassword() {
	password := "invalid-password"
	key, _ := crypto.GenerateKey()
	account := &common.SelectedExtKey{
		Address:    common.FromAddress(TestConfig.Account1.Address),
		AccountKey: &keystore.Key{PrivateKey: key},
	}
	s.setupStatusBackend(account, password)

	nonce := hexutil.Uint64(10)
	gas := hexutil.Big(*big.NewInt(defaultGas + 1))
	s.setupTransactionPoolAPI(account, nonce, gas, keystore.ErrDecrypt)

	txQueueManager := NewManager(s.nodeManagerMock, s.accountManagerMock)
	txQueueManager.DisableNotificactions()

	txQueueManager.Start()
	defer txQueueManager.Stop()

	tx := common.CreateTransaction(context.Background(), common.SendTxArgs{
		From: common.FromAddress(TestConfig.Account1.Address),
		To:   common.ToAddress(TestConfig.Account2.Address),
	})

	txQueueManager.QueueTransaction(tx)

	_, err := txQueueManager.CompleteTransaction(tx.ID, password)
	s.Equal(err.Error(), keystore.ErrDecrypt.Error())

	// Transaction should stay in the queue as mismatched accounts
	// is a recoverable error.
	s.True(txQueueManager.TransactionQueue().Has(tx.ID))
}

func (s *TxQueueTestSuite) TestDiscardTransaction() {
	txQueueManager := NewManager(s.nodeManagerMock, s.accountManagerMock)
	txQueueManager.DisableNotificactions()

	txQueueManager.Start()
	defer txQueueManager.Stop()

	tx := common.CreateTransaction(context.Background(), common.SendTxArgs{
		From: common.FromAddress(TestConfig.Account1.Address),
		To:   common.ToAddress(TestConfig.Account2.Address),
	})

	c := txQueueManager.QueueTransaction(tx)
	w := make(chan struct{})
	go func() {
		discardErr := txQueueManager.DiscardTransaction(tx.ID)
		s.NoError(discardErr)
		close(w)
	}()

	rst := txQueueManager.WaitForTransaction(tx, c)
	s.Equal(queue.ErrQueuedTxDiscarded, rst.Err)
	// Transaction should be already removed from the queue.
	s.False(txQueueManager.TransactionQueue().Has(tx.ID))
	<-w
}
