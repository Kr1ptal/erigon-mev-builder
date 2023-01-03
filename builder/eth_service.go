package builder

import (
	"bytes"
	"context"
	metrics2 "github.com/VictoriaMetrics/metrics"
	"github.com/ledgerwatch/erigon-lib/common/hexutility"
	"github.com/ledgerwatch/erigon/turbo/builder"

	"github.com/ledgerwatch/erigon-lib/chain"
	"github.com/ledgerwatch/erigon-lib/common"
	hexutil2 "github.com/ledgerwatch/erigon-lib/common/hexutil"
	"github.com/ledgerwatch/erigon-lib/kv"
	libstate "github.com/ledgerwatch/erigon-lib/state"
	txpool2 "github.com/ledgerwatch/erigon-lib/txpool"
	"github.com/ledgerwatch/erigon/consensus"
	"github.com/ledgerwatch/erigon/core"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/core/vm"
	"github.com/ledgerwatch/erigon/eth/ethconfig"
	"github.com/ledgerwatch/erigon/eth/stagedsync"
	"github.com/ledgerwatch/erigon/eth/stagedsync/stages"
	"github.com/ledgerwatch/erigon/event"
	"github.com/ledgerwatch/erigon/mev"
	"github.com/ledgerwatch/erigon/params"
	"github.com/ledgerwatch/erigon/turbo/engineapi/engine_types"
	"github.com/ledgerwatch/erigon/turbo/services"
	turbostages "github.com/ledgerwatch/erigon/turbo/stages"
	"github.com/ledgerwatch/log/v3"
	"time"
)

var buildBlockTimer = metrics2.GetOrCreateSummary("build_block_seconds")

type IEthereum interface {
	SentryCtx() context.Context
	ChainDB() kv.RwDB
	ChainConfig() *chain.Config
	Engine() consensus.Engine
	Config() *ethconfig.Config
	Agg() *libstate.AggregatorV3
	BlockReader() services.FullBlockReader
	TxPool2() *txpool2.TxPool
	TxPool2DB() kv.RwDB
}

type IEthereumService interface {
	BuildBlock(attrs *BuilderPayloadAttributes, builderParams *BuilderParams, interrupt *int32) (*engine_types.ExecutionPayload, *types.Block)
	GetBlockByHash(hash common.Hash) *types.Block
	Synced() bool
}

type testEthereumService struct {
	synced             bool
	testExecutableData *engine_types.ExecutionPayload
	testBlock          *types.Block
	testTxPool         *testTxPool
}

func (t *testEthereumService) BuildBlock(attrs *BuilderPayloadAttributes, builderParams *BuilderParams, interrupt *int32) (*engine_types.ExecutionPayload, *types.Block) {
	return t.testExecutableData, t.testBlock
}

func (t *testEthereumService) GetBlockByHash(hash common.Hash) *types.Block { return t.testBlock }

func (t *testEthereumService) Synced() bool { return t.synced }

type testTxPool struct {
	txFeed     event.Feed
	bundleFeed event.Feed
}

func (t *testTxPool) SubscribeNewTxsEvent(ch chan<- core.NewTxsEvent) event.Subscription {
	return t.txFeed.Subscribe(ch)
}

type EthereumService struct {
	backend    IEthereum
	bundlePool *mev.BundlePool
	logger     log.Logger
}

func NewEthereumService(backend IEthereum, bundlePool *mev.BundlePool, logger log.Logger) *EthereumService {
	return &EthereumService{
		backend:    backend,
		bundlePool: bundlePool,
		logger:     logger,
	}
}

func (s *EthereumService) BuildBlock(attrs *BuilderPayloadAttributes, builderParams *BuilderParams, interrupt *int32) (*engine_types.ExecutionPayload, *types.Block) {
	startTime := time.Now()

	timer := time.NewTimer(4 * time.Second)
	defer timer.Stop()
	go func() {
		select {
		case <-timer.C:
			log.Error("timeout waiting for block", "parent hash", attrs.HeadHash, "slot", attrs.Slot)
		}
	}()

	state := stagedsync.NewProposingState(
		&params.MiningConfig{
			Etherbase: builderParams.FeeRecipient,
			GasLimit:  attrs.GasLimit,
			ExtraData: nil,
		},
	)

	param := &core.BlockBuilderParameters{
		ParentHash:            attrs.HeadHash,
		Timestamp:             uint64(attrs.Timestamp),
		PrevRandao:            attrs.Random,
		SuggestedFeeRecipient: attrs.SuggestedFeeRecipient,
		Withdrawals:           attrs.Withdrawals,
		PayloadId:             attrs.Slot,
	}

	latestBlockBuiltStore := builder.NewLatestBlockBuiltStore()
	dirs := s.backend.Config().Dirs
	tmpdir := dirs.Tmp
	sync := stagedsync.New(
		BuilderStages(
			s.backend.SentryCtx(),
			stagedsync.StageMiningCreateBlockCfg(s.backend.ChainDB(), state, *s.backend.ChainConfig(), s.backend.Engine(), nil, param, tmpdir, s.backend.BlockReader()),
			StageBuilderExecConfig(builderParams, &attrs.SuggestedFeeRecipient, state, *s.backend.ChainConfig(), s.backend.Engine(), &vm.Config{}, interrupt, param.PayloadId, s.backend.TxPool2(), s.backend.TxPool2DB(), s.bundlePool, s.backend.BlockReader()),
			stagedsync.StageHashStateCfg(s.backend.ChainDB(), dirs, s.backend.Config().HistoryV3),
			stagedsync.StageTrieCfg(s.backend.ChainDB(), false, true, true, tmpdir, s.backend.BlockReader(), nil, s.backend.Config().HistoryV3, s.backend.Agg()),
			stagedsync.StageMiningFinishCfg(s.backend.ChainDB(), *s.backend.ChainConfig(), s.backend.Engine(), state, nil, s.backend.BlockReader(), latestBlockBuiltStore),
		),
		stagedsync.MiningUnwindOrder,
		stagedsync.MiningPruneOrder,
		s.logger,
	)

	if err := turbostages.MiningStep(s.backend.SentryCtx(), s.backend.ChainDB(), sync, tmpdir); err != nil {
		log.Error("error while building block", "parent hash", attrs.HeadHash, "slot", attrs.Slot, "err", err)
		return nil, nil
	}
	block := (<-state.MiningResultPOSCh).Block

	transactions := make([]hexutility.Bytes, len(block.Transactions()))
	for i, transaction := range block.Transactions() {
		var buf bytes.Buffer
		_ = transaction.MarshalBinary(&buf)
		transactions[i] = buf.Bytes()
	}

	// update only after block has been built successfully
	buildBlockTimer.UpdateDuration(startTime)
	var blobGasUsed *hexutil2.Uint64 = nil
	if block.Header().BlobGasUsed != nil {
		tmp := hexutil2.Uint64(*block.Header().BlobGasUsed)
		blobGasUsed = &tmp
	}
	var excessBlobGas *hexutil2.Uint64 = nil
	if block.Header().ExcessBlobGas != nil {
		tmp := hexutil2.Uint64(*block.Header().ExcessBlobGas)
		excessBlobGas = &tmp
	}

	return &engine_types.ExecutionPayload{
		ParentHash:    block.ParentHash(),
		FeeRecipient:  block.Coinbase(),
		StateRoot:     block.Root(),
		ReceiptsRoot:  block.ReceiptHash(),
		LogsBloom:     block.Bloom().Bytes(),
		PrevRandao:    block.MixDigest(),
		BlockNumber:   hexutil2.Uint64(block.NumberU64()),
		GasLimit:      hexutil2.Uint64(block.GasLimit()),
		GasUsed:       hexutil2.Uint64(block.GasUsed()),
		Timestamp:     hexutil2.Uint64(block.Time()),
		ExtraData:     block.Extra(),
		BaseFeePerGas: (*hexutil2.Big)(block.BaseFee()),
		BlockHash:     block.Hash(),
		Transactions:  transactions,
		Withdrawals:   block.Withdrawals(),
		BlobGasUsed:   blobGasUsed,
		ExcessBlobGas: excessBlobGas,
	}, block
}

func (s *EthereumService) GetBlockByHash(hash common.Hash) *types.Block {
	dbTx, err := s.backend.ChainDB().BeginRo(context.Background())
	if err != nil {
		return nil
	}
	defer dbTx.Rollback()
	header, err := s.backend.BlockReader().HeaderByHash(context.Background(), dbTx, hash)
	if err != nil || header == nil {
		return nil
	}
	block, _, _ := s.backend.BlockReader().BlockWithSenders(context.Background(), dbTx, hash, header.Number.Uint64())
	return block
}

func (s *EthereumService) Synced() bool {
	dbTx, err := s.backend.ChainDB().BeginRo(context.Background())
	if err != nil {
		return false
	}
	defer dbTx.Rollback()
	highestBlock, err := stages.GetStageProgress(dbTx, stages.Headers)
	if err != nil {
		return false
	}

	currentBlock, err := stages.GetStageProgress(dbTx, stages.Finish)
	if err != nil {
		return false
	}

	return currentBlock > 0 && currentBlock >= highestBlock
}
