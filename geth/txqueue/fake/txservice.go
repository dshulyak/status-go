package fake

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
	gomock "github.com/golang/mock/gomock"
)

func NewTestServer(ctrl *gomock.Controller) (*rpc.Server, *FakePublicTransactionPoolAPI) {
	srv := rpc.NewServer()
	svc := NewMockFakePublicTxApi(ctrl)
	if err := srv.RegisterName("eth", svc); err != nil {
		panic(err)
	}
	return srv, svc
}

// CallArgs copied from module go-ethereum/internal/ethapi
type CallArgs struct {
	From     common.Address  `json:"from"`
	To       *common.Address `json:"to"`
	Gas      hexutil.Big     `json:"gas"`
	GasPrice hexutil.Big     `json:"gasPrice"`
	Value    hexutil.Big     `json:"value"`
	Data     hexutil.Bytes   `json:"data"`
}
