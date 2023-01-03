package builder

import (
	"context"
	"github.com/ledgerwatch/erigon-lib/chain"
	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/common/hexutil"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon-lib/kv/memdb"
	libstate "github.com/ledgerwatch/erigon-lib/state"
	"github.com/ledgerwatch/erigon-lib/txpool"
	"github.com/ledgerwatch/erigon/consensus"
	"github.com/ledgerwatch/erigon/consensus/ethash"
	"github.com/ledgerwatch/erigon/core"
	"github.com/ledgerwatch/erigon/core/rawdb"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/crypto"
	"github.com/ledgerwatch/erigon/eth/ethconfig"
	"github.com/ledgerwatch/erigon/mev"
	"github.com/ledgerwatch/erigon/node/nodecfg"
	"github.com/ledgerwatch/erigon/params"
	"github.com/ledgerwatch/erigon/turbo/services"
	"github.com/ledgerwatch/erigon/turbo/stages/mock"
	"github.com/ledgerwatch/erigon/turbo/testlog"
	"github.com/ledgerwatch/log/v3"
	"github.com/stretchr/testify/require"
	"math/big"
	"testing"
)

type backend struct {
	sentry *mock.MockSentry
}

func (b *backend) SentryCtx() context.Context {
	return b.sentry.Ctx
}

func (b *backend) ChainDB() kv.RwDB {
	return b.sentry.DB
}

func (b *backend) ChainConfig() *chain.Config {
	return b.sentry.ChainConfig
}

func (b *backend) Engine() consensus.Engine {
	return b.sentry.Engine
}

func (b *backend) Config() *ethconfig.Config {
	cfg := ethconfig.Defaults
	cfg.Dirs = nodecfg.DefaultConfig.Dirs

	return &cfg
}

func (b *backend) Agg() *libstate.AggregatorV3 {
	return b.sentry.HistoryV3Components()
}

func (b *backend) BlockReader() services.FullBlockReader {
	return b.sentry.BlockReader
}

func (b *backend) TxPool2() *txpool.TxPool {
	return b.sentry.TxPool
}

func (b *backend) TxPool2DB() kv.RwDB {
	return b.sentry.TxPoolDB()
}

func generatePreMergeChain(t *testing.T, n int) *backend {
	db := memdb.New(t.TempDir())
	config := params.AllProtocolChanges
	genesis := &types.Genesis{
		Config:     config,
		Alloc:      types.GenesisAlloc{},
		ExtraData:  []byte("test genesis"),
		Timestamp:  9000,
		BaseFee:    big.NewInt(params.InitialBaseFee),
		Difficulty: big.NewInt(0),
	}
	gblock, _, _ := core.GenesisToBlock(genesis, t.TempDir())
	engine := ethash.NewFaker()
	blocks, _ := core.GenerateChain(config, gblock, engine, db, n, nil)
	totalDifficulty := big.NewInt(0)
	for _, b := range blocks.Blocks {
		totalDifficulty.Add(totalDifficulty, b.Difficulty())
	}
	config.TerminalTotalDifficulty = totalDifficulty
	m := mock.MockWithGenesisEngineTxPool(t, genesis, engine, false)
	m.InsertChain(blocks)

	return &backend{m}
}

func TestBuildBlock(t *testing.T) {
	b := generatePreMergeChain(t, 10)

	var parent *types.Header
	b.ChainDB().View(context.Background(), func(tx kv.Tx) error {
		parent = rawdb.ReadCurrentHeader(tx)
		return nil
	})

	testPayloadAttributes := &BuilderPayloadAttributes{
		Timestamp:             hexutil.Uint64(parent.Time + 1),
		Random:                common.Hash{0x05, 0x10},
		SuggestedFeeRecipient: common.Address{0x04, 0x10},
		GasLimit:              uint64(4800000),
		Slot:                  uint64(25),
		HeadHash:              parent.Hash(),
	}
	pk, _ := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	builderParams := &BuilderParams{
		FeeRecipient:   crypto.PubkeyToAddress(pk.PublicKey),
		FeeRecipientPk: pk,
		PaymentPercent: big.NewInt(100),
	}

	service := NewEthereumService(b, mev.NewBundlePool(), testlog.Logger(t, log.LvlInfo))
	executableData, block := service.BuildBlock(testPayloadAttributes, builderParams, nil)

	//require.Equal(t, common.Address{0x04, 0x10}, executableData.FeeRecipient)
	//require.Equal(t, common.Hash{0x05, 0x10}, executableData.PrevRandao)
	require.Equal(t, parent.Hash(), executableData.ParentHash)
	//require.Equal(t, parent.Time()+1, executableData.Timestamp)
	require.Equal(t, block.ParentHash(), parent.Hash())
	require.Equal(t, block.Hash(), executableData.BlockHash)
	require.Equal(t, block.Profit.Uint64(), uint64(0))
}
