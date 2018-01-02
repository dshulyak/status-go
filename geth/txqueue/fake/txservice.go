package fake

import (
	"context"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/stretchr/testify/mock"
)

func NewTestServer() (*rpc.Server, *FakePublicTransactionPoolAPI) {
	srv := rpc.NewServer()
	svc := &FakePublicTransactionPoolAPI{mock.Mock{}}
	if err := srv.RegisterName("eth", svc); err != nil {
		panic(err)
	}
	return srv, svc
}

type FakePublicTransactionPoolAPI struct {
	mock.Mock
}

func (e *FakePublicTransactionPoolAPI) GasPrice(ctx context.Context) (*big.Int, error) {
	calledArgs := e.Called(ctx)
	return calledArgs.Get(0).(*big.Int), calledArgs.Error(1)
}

func (e *FakePublicTransactionPoolAPI) EstimateGas(ctx context.Context, args CallArgs) (*hexutil.Big, error) {
	calledArgs := e.Called(ctx, args)
	return calledArgs.Get(0).(*hexutil.Big), calledArgs.Error(1)
}

func (e *FakePublicTransactionPoolAPI) GetTransactionCount(ctx context.Context, address common.Address, blockNr rpc.BlockNumber) (*hexutil.Uint64, error) {
	calledArgs := e.Called(ctx, address, blockNr)
	return calledArgs.Get(0).(*hexutil.Uint64), calledArgs.Error(1)
}

func (e *FakePublicTransactionPoolAPI) SendRawTransaction(ctx context.Context, encodedTx hexutil.Bytes) (common.Hash, error) {
	calledArgs := e.Called(ctx, encodedTx)
	return calledArgs.Get(0).(common.Hash), calledArgs.Error(1)
}

// Copied from module go-ethereum/internal/ethapi
type CallArgs struct {
	From     common.Address  `json:"from"`
	To       *common.Address `json:"to"`
	Gas      hexutil.Big     `json:"gas"`
	GasPrice hexutil.Big     `json:"gasPrice"`
	Value    hexutil.Big     `json:"value"`
	Data     hexutil.Bytes   `json:"data"`
}
