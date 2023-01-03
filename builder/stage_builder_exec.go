package builder

import (
	"errors"
	"fmt"
	mapset "github.com/deckarep/golang-set/v2"
	"github.com/ledgerwatch/erigon-lib/chain"
	"github.com/ledgerwatch/erigon/eth/stagedsync"
	"github.com/ledgerwatch/erigon/eth/tracers/logger"
	"github.com/ledgerwatch/erigon/mev"
	"io"
	"math/big"
	"sort"
	"sync/atomic"
	"time"

	"github.com/holiman/uint256"
	"github.com/ledgerwatch/log/v3"
	"golang.org/x/net/context"

	libcommon "github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon-lib/txpool"
	types2 "github.com/ledgerwatch/erigon-lib/types"
	"github.com/ledgerwatch/erigon/core/rawdb"
	"github.com/ledgerwatch/erigon/core/types/accounts"
	"github.com/ledgerwatch/erigon/turbo/services"

	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon/consensus"
	"github.com/ledgerwatch/erigon/consensus/misc"
	"github.com/ledgerwatch/erigon/core"
	"github.com/ledgerwatch/erigon/core/state"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/core/vm"
	"github.com/ledgerwatch/erigon/eth/stagedsync/stages"
	"github.com/ledgerwatch/erigon/params"
)

const (
	paymentTxGas = 26000
)

type mevBundleSimulation struct {
	bundle *types.MevBundle

	receipts          types.Receipts
	mevGasPrice       *big.Int
	totalEth          *big.Int
	ethSentToCoinbase *big.Int
	totalGasUsed      uint64
}

type BuilderExecConfig struct {
	builder           *BuilderParams
	validatorCoinbase *common.Address
	state             stagedsync.MiningState
	chainConfig       chain.Config
	engine            consensus.Engine
	blockReader       services.FullBlockReader
	vmConfig          *vm.Config
	interrupt         *int32
	payloadId         uint64
	txPool2           *txpool.TxPool
	txPool2DB         kv.RoDB
	bundlePool        *mev.BundlePool
}

func StageBuilderExecConfig(
	builder *BuilderParams,
	validatorCoinbase *common.Address,
	state stagedsync.MiningState,
	chainConfig chain.Config,
	engine consensus.Engine,
	vmConfig *vm.Config,
	interrupt *int32,
	payloadId uint64,
	txPool2 *txpool.TxPool,
	txPool2DB kv.RoDB,
	bundlePool *mev.BundlePool,
	blockReader services.FullBlockReader,
) BuilderExecConfig {
	return BuilderExecConfig{
		builder:           builder,
		validatorCoinbase: validatorCoinbase,
		state:             state,
		chainConfig:       chainConfig,
		engine:            engine,
		blockReader:       blockReader,
		vmConfig:          vmConfig,
		interrupt:         interrupt,
		payloadId:         payloadId,
		txPool2:           txPool2,
		txPool2DB:         txPool2DB,
		bundlePool:        bundlePool,
	}
}

func SpawnBuilderExecStage(s *stagedsync.StageState, tx kv.RwTx, cfg BuilderExecConfig, quit <-chan struct{}, logger log.Logger) error {
	cfg.vmConfig.NoReceipts = false
	chainID, _ := uint256.FromBig(cfg.chainConfig.ChainID)
	logPrefix := s.LogPrefix()
	current := cfg.state.MiningBlock

	stateReader := state.NewPlainStateReader(tx)
	ibs := state.New(stateReader)
	stateWriter := state.NewPlainStateWriter(tx, tx, current.Header.Number.Uint64())
	if cfg.chainConfig.DAOForkBlock != nil && cfg.chainConfig.DAOForkBlock.Cmp(current.Header.Number) == 0 {
		misc.ApplyDAOHardFork(ibs)
	}
	chainReader := stagedsync.ChainReader{Cfg: cfg.chainConfig, Db: tx, BlockReader: cfg.blockReader}
	core.InitializeBlockExecution(cfg.engine, chainReader, current.Header, &cfg.chainConfig, ibs, logger)

	// need to clone the value because a pointer to it is returned - thus it get overridden on each update
	coinbaseBalanceBefore := ibs.GetBalance(current.Header.Coinbase).Clone()

	yielded := mapset.NewSet[[32]byte]()
	senderAccountCache := make(map[common.Address]*accounts.Account)

	executionAt, err := s.ExecutionAt(tx)
	if err != nil {
		return err
	}

	txpoolTx, err := cfg.txPool2DB.BeginRo(context.Background())
	if err != nil {
		return err
	}
	defer txpoolTx.Rollback()

	signer := types.MakeSigner(&cfg.chainConfig, current.Header.Number.Uint64(), current.Header.Time)

	gasUsed := uint64(0)
	blobGasUsed := uint64(0)
	env := &environment{
		signer:        *signer,
		engine:        cfg.engine,
		chainConfig:   &cfg.chainConfig,
		vmConfig:      cfg.vmConfig,
		verifier:      cfg.builder.Verifier,
		getHeaderFunc: func(hash common.Hash, number uint64) *types.Header { return rawdb.ReadHeader(tx, hash, number) },
		block:         current,
		txpool:        cfg.txPool2,
		txpoolTx:      txpoolTx,
		state:         ibs,
		gasPool:       new(core.GasPool).AddGas(current.Header.GasLimit - paymentTxGas),
		txs:           types.Transactions{},
		receipts:      types.Receipts{},
		gasUsed:       &gasUsed,
		blobGasUsed:   &blobGasUsed,
	}

	bundles := cfg.bundlePool.MevBundles(current.Header.Number, current.Header.Time)
	if bundles != nil && len(bundles) > 0 {
		simulatedBundles := make([]*mevBundleSimulation, len(bundles))

		for _, bundle := range bundles {
			bundleEnv := env.clone()

			sim, err := bundleEnv.executeBundle(bundle)
			if err != nil {
				continue
			}

			simulatedBundles = append(simulatedBundles, sim)
		}

		// sort descending based on total gas price
		sort.SliceStable(simulatedBundles, func(i, j int) bool {
			return simulatedBundles[j].mevGasPrice.Cmp(simulatedBundles[i].mevGasPrice) < 0
		})

		currentEnv := env.clone()
		for _, simulated := range simulatedBundles {
			bundleEnv := currentEnv.clone()

			// the floor gas price is 99/100 what was simulated at the top of the block
			floorGasPrice := new(big.Int).Mul(simulated.mevGasPrice, big.NewInt(99))
			floorGasPrice = floorGasPrice.Div(floorGasPrice, big.NewInt(100))

			sim, err := bundleEnv.commitBundle(simulated.bundle)
			if err != nil || sim.mevGasPrice.Cmp(floorGasPrice) < 0 {
				continue
			}

			currentEnv = bundleEnv
		}

		env = currentEnv
	}

	for {
		txs, y, err := getNextTransactions(cfg, chainID, current.Header, 50, executionAt, tx, senderAccountCache, yielded)
		if err != nil {
			return err
		}

		if !txs.Empty() {
			stop, err := addTransactionsToMiningBlock(env, logPrefix, txs, quit, cfg.interrupt, cfg.payloadId)
			if err != nil {
				return err
			}
			if stop {
				break
			}
		} else {
			break
		}

		// if we yielded less than the count we wanted, assume the txpool has run dry now and stop to save another loop
		if y < 50 {
			break
		}
	}

	coinbaseBalanceAfter := env.state.GetBalance(current.Header.Coinbase)
	current.Profit = uint256.NewInt(0).Sub(coinbaseBalanceAfter, coinbaseBalanceBefore).ToBig()

	if current.Profit.Sign() == 1 {
		payoutTx, sentToValidator, err := createProposerPayoutTx(&cfg, env, cfg.validatorCoinbase, current.Profit)
		if err != nil {
			return err
		}

		env.gasPool.AddGas(paymentTxGas)
		err = env.commitTransaction(payoutTx)
		if err != nil {
			return err
		}
		current.Profit = sentToValidator
	}

	current.Header.GasUsed = *env.gasUsed
	current.Header.BlobGasUsed = &(*env.blobGasUsed)
	current.Txs = env.txs
	current.Receipts = env.receipts

	log.Debug("SpawnMiningExecStage", "block txn", current.Txs.Len(), "payload", cfg.payloadId)
	if current.Uncles == nil {
		current.Uncles = []*types.Header{}
	}
	if current.Txs == nil {
		current.Txs = []types.Transaction{}
	}
	if current.Receipts == nil {
		current.Receipts = types.Receipts{}
	}

	_, current.Txs, current.Receipts, err = core.FinalizeBlockExecution(
		cfg.engine,
		stateReader,
		current.Header,
		current.Txs,
		current.Uncles,
		stateWriter,
		&cfg.chainConfig,
		env.state,
		current.Receipts,
		current.Withdrawals,
		stagedsync.NewChainReaderImpl(&cfg.chainConfig, tx, cfg.blockReader, logger),
		true,
		logger,
	)
	if err != nil {
		return err
	}
	log.Debug("FinalizeBlockExecution", "current txn", current.Txs.Len(), "current receipt", current.Receipts.Len(), "payload", cfg.payloadId)

	// hack: pretend that we are real execution stage - next stages will rely on this progress
	if err := stages.SaveStageProgress(tx, stages.Execution, current.Header.Number.Uint64()); err != nil {
		return err
	}
	return nil
}

func getNextTransactions(
	cfg BuilderExecConfig,
	chainID *uint256.Int,
	header *types.Header,
	amount uint16,
	executionAt uint64,
	tx kv.Tx,
	senderAccountCache map[common.Address]*accounts.Account,
	alreadyYielded mapset.Set[[32]byte],
) (types.TransactionsStream, int, error) {
	txSlots := types2.TxsRlp{}
	var onTime bool
	var yielded int
	if err := cfg.txPool2DB.View(context.Background(), func(poolTx kv.Tx) error {
		var err error
		counter := 0
		for {
			remainingGas := header.GasLimit - header.GasUsed
			var remainingBlobGas uint64 = 0
			if header.ExcessBlobGas != nil {
				remainingBlobGas = *header.ExcessBlobGas - *header.BlobGasUsed
			}
			if onTime, yielded, err = cfg.txPool2.YieldBest(amount, &txSlots, poolTx, executionAt, remainingGas, remainingBlobGas, alreadyYielded); err != nil {
				return err
			}

			if onTime || counter >= 1000 {
				break
			}

			time.Sleep(1 * time.Millisecond)
			counter++
		}
		return nil
	}); err != nil {
		return nil, 0, err
	}

	var txs []types.Transaction //nolint:prealloc
	for i := range txSlots.Txs {
		transaction, err := types.DecodeTransaction(txSlots.Txs[i])
		if err == io.EOF {
			continue
		}
		if err != nil {
			return nil, 0, err
		}
		if !transaction.GetChainID().IsZero() && transaction.GetChainID().Cmp(chainID) != 0 {
			continue
		}

		var sender common.Address
		copy(sender[:], txSlots.Senders.At(i))

		// Check if tx nonce is too low
		txs = append(txs, transaction)
		txs[len(txs)-1].SetSender(sender)
	}

	blockNum := executionAt + 1
	txs, err := filterBadTransactions(txs, cfg.chainConfig, blockNum, header.BaseFee, tx, senderAccountCache, cfg.builder.Verifier)
	if err != nil {
		return nil, 0, err
	}

	return types.NewTransactionsFixedOrder(txs), yielded, nil
}

func filterBadTransactions(
	transactions []types.Transaction,
	config chain.Config,
	blockNumber uint64,
	baseFee *big.Int,
	tx kv.Tx,
	// TODO this does not take into account bundle txs.
	senderAccountCache map[common.Address]*accounts.Account,
	verifier *AccessVerifier,
) ([]types.Transaction, error) {
	initialCnt := len(transactions)
	var filtered []types.Transaction

	startTime := time.Now()
	verifierBlocked := 0
	missedTxs := 0
	noSenderCnt := 0
	noAccountCnt := 0
	nonceTooLowCnt := 0
	notEOACnt := 0
	feeTooLowCnt := 0
	balanceTooLowCnt := 0
	overflowCnt := 0
	for len(transactions) > 0 && missedTxs != len(transactions) {
		transaction := transactions[0]

		sender, ok := transaction.GetSender()
		if !ok {
			transactions = transactions[1:]
			noSenderCnt++
			continue
		}

		if verifier != nil {
			to := transaction.GetTo()
			if verifier.verifyAddress(sender) != nil || (to != nil && verifier.verifyAddress(*to) != nil) {
				transactions = transactions[1:]
				verifierBlocked++
				continue
			}
		}

		account, inCache := senderAccountCache[sender]
		if !inCache {
			account = &accounts.Account{}
			ok, err := rawdb.ReadAccount(tx, sender, account)
			if err != nil {
				return nil, err
			}
			if !ok {
				transactions = transactions[1:]
				noAccountCnt++
				continue
			}
		}

		// Check transaction nonce
		if account.Nonce > transaction.GetNonce() {
			transactions = transactions[1:]
			nonceTooLowCnt++
			continue
		}
		if account.Nonce < transaction.GetNonce() {
			missedTxs++
			transactions = append(transactions[1:], transaction)
			continue
		}
		missedTxs = 0

		// Make sure the sender is an EOA (EIP-3607)
		if !account.IsEmptyCodeHash() {
			transactions = transactions[1:]
			notEOACnt++
			continue
		}

		if config.IsLondon(blockNumber) {
			baseFee256 := uint256.NewInt(0)
			if overflow := baseFee256.SetFromBig(baseFee); overflow {
				return nil, fmt.Errorf("bad baseFee %s", baseFee)
			}
			// Make sure the transaction gasFeeCap is greater than the block's baseFee.
			if !transaction.GetFeeCap().IsZero() || !transaction.GetTip().IsZero() {
				if err := core.CheckEip1559TxGasFeeCap(sender, transaction.GetFeeCap(), transaction.GetTip(), baseFee256, false /* isFree */); err != nil {
					transactions = transactions[1:]
					feeTooLowCnt++
					continue
				}
			}
		}
		txnGas := transaction.GetGas()
		txnPrice := transaction.GetPrice()
		value := transaction.GetValue()
		accountBalance := account.Balance

		want := uint256.NewInt(0)
		want.SetUint64(txnGas)
		want, overflow := want.MulOverflow(want, txnPrice)
		if overflow {
			transactions = transactions[1:]
			overflowCnt++
			continue
		}

		if transaction.GetFeeCap() != nil {
			want.SetUint64(txnGas)
			want, overflow = want.MulOverflow(want, transaction.GetFeeCap())
			if overflow {
				transactions = transactions[1:]
				overflowCnt++
				continue
			}
			want, overflow = want.AddOverflow(want, value)
			if overflow {
				transactions = transactions[1:]
				overflowCnt++
				continue
			}
		}

		if accountBalance.Cmp(want) < 0 {
			transactions = transactions[1:]
			balanceTooLowCnt++
			continue
		}
		// Updates account in the simulation
		account.Nonce++
		account.Balance.Sub(&account.Balance, want)
		senderAccountCache[sender] = account

		// Mark transaction as valid
		filtered = append(filtered, transaction)
		transactions = transactions[1:]
	}
	log.Debug("Filtration", "duration", time.Since(startTime), "initial", initialCnt, "verifier blocked", verifierBlocked, "no sender", noSenderCnt, "no account", noAccountCnt, "nonce too low", nonceTooLowCnt, "nonceTooHigh", missedTxs, "sender not EOA", notEOACnt, "fee too low", feeTooLowCnt, "overflow", overflowCnt, "balance too low", balanceTooLowCnt, "filtered", len(filtered))
	return filtered, nil
}

func addTransactionsToMiningBlock(env *environment, logPrefix string, txs types.TransactionsStream, quit <-chan struct{}, interrupt *int32, payloadId uint64) (bool, error) {
	var stopped *time.Ticker
	defer func() {
		if stopped != nil {
			stopped.Stop()
		}
	}()

	done := false

LOOP:
	for {
		// see if we need to stop now
		if stopped != nil {
			select {
			case <-stopped.C:
				done = true
				break LOOP
			default:
			}
		}

		if err := libcommon.Stopped(quit); err != nil {
			return true, err
		}

		if interrupt != nil && atomic.LoadInt32(interrupt) != 0 && stopped == nil {
			log.Debug("Transaction adding was requested to stop", "payload", payloadId)
			// ensure we run for at least 500ms after the request to stop comes in from GetPayload
			stopped = time.NewTicker(500 * time.Millisecond)
		}
		// If we don't have enough gas for any further transactions then we're done
		if env.gasPool.Gas() < params.TxGas {
			log.Debug(fmt.Sprintf("[%s] Not enough gas for further transactions", logPrefix), "have", env.gasPool, "want", params.TxGas)
			done = true
			break
		}
		// Retrieve the next transaction and abort if all done
		txn := txs.Peek()
		if txn == nil {
			break
		}

		// We use the eip155 signer regardless of the env hf.
		from, err := txn.Sender(env.signer)
		if err != nil {
			log.Warn(fmt.Sprintf("[%s] Could not recover transaction sender", logPrefix), "hash", txn.Hash(), "err", err)
			txs.Pop()
			continue
		}

		// Check whether the txn is replay protected. If we're not in the EIP155 (Spurious Dragon) hf
		// phase, start ignoring the sender until we do.
		if txn.Protected() && !env.chainConfig.IsSpuriousDragon(env.block.Header.Number.Uint64()) {
			log.Debug(fmt.Sprintf("[%s] Ignoring replay protected transaction", logPrefix), "hash", txn.Hash(), "eip155", env.chainConfig.SpuriousDragonBlock)

			txs.Pop()
			continue
		}

		// Start executing the transaction
		err = env.commitTransaction(txn)

		if errors.Is(err, core.ErrGasLimitReached) {
			// Pop the env out-of-gas transaction without shifting in the next from the account
			log.Debug(fmt.Sprintf("[%s] Gas limit exceeded for env block", logPrefix), "hash", txn.Hash(), "sender", from)
			txs.Pop()
		} else if errors.Is(err, core.ErrNonceTooLow) {
			// New head notification data race between the transaction pool and miner, shift
			log.Debug(fmt.Sprintf("[%s] Skipping transaction with low nonce", logPrefix), "hash", txn.Hash(), "sender", from, "nonce", txn.GetNonce())
			txs.Shift()
		} else if errors.Is(err, core.ErrNonceTooHigh) {
			// Reorg notification data race between the transaction pool and miner, skip account =
			log.Debug(fmt.Sprintf("[%s] Skipping transaction with high nonce", logPrefix), "hash", txn.Hash(), "sender", from, "nonce", txn.GetNonce())
			txs.Pop()
		} else if err == nil {
			// Everything ok, collect the logs and shift in the next transaction from the same account
			log.Debug(fmt.Sprintf("[%s] addTransactionsToMiningBlock Successful", logPrefix), "sender", from, "nonce", txn.GetNonce(), "payload", payloadId)
			txs.Shift()
		} else {
			// Strange error, discard the transaction and get the next in line (note, the
			// nonce-too-high clause will prevent us from executing in vain).
			log.Error(fmt.Sprintf("[%s] Skipping transaction", logPrefix), "hash", txn.Hash(), "sender", from, "err", err)
			if errors.Is(err, errBlacklisted) {
				txs.Pop()
			} else {
				txs.Shift()
			}
		}
	}

	return done, nil
}

func createProposerPayoutTx(cfg *BuilderExecConfig, env *environment, recipient *common.Address, profit *big.Int) (types.Transaction, *big.Int, error) {
	builder := cfg.builder.FeeRecipient
	sender := builder.Hex()
	nonce := env.state.GetNonce(builder)

	fee := new(big.Int).Mul(big.NewInt(paymentTxGas), env.block.Header.BaseFee)
	amount := new(big.Int).Sub(profit, fee)
	if amount.Sign() != 1 {
		return nil, nil, errors.New("negative or zero amount of proposer payout")
	}

	paymentAmount := new(big.Int).Div(new(big.Int).Mul(amount, cfg.builder.PaymentPercent), big.NewInt(100))
	paymentAmountInt, _ := uint256.FromBig(paymentAmount)

	gasPrice := new(big.Int).Set(env.block.Header.BaseFee)
	gasPriceInt, _ := uint256.FromBig(gasPrice)

	chainId := cfg.chainConfig.ChainID
	log.Debug("createProposerPayoutTx", "sender", sender, "chainId", chainId.String(), "nonce", nonce, "amountTotal", amount.String(), "amountPaid", paymentAmount.String(), "baseFee", env.block.Header.BaseFee.String(), "fee", fee)
	tx := types.NewTransaction(nonce, *recipient, paymentAmountInt, paymentTxGas, gasPriceInt, nil)

	signed, err := types.SignTx(tx, env.signer, cfg.builder.FeeRecipientPk)
	if err != nil {
		return nil, nil, err
	}
	return signed, paymentAmount, nil
}

type environment struct {
	// immutable
	signer        types.Signer
	engine        consensus.Engine
	chainConfig   *chain.Config
	vmConfig      *vm.Config
	verifier      *AccessVerifier
	getHeaderFunc func(hash common.Hash, number uint64) *types.Header
	block         *stagedsync.MiningBlock
	txpool        *txpool.TxPool
	txpoolTx      kv.Tx

	// mutable
	state       *state.IntraBlockState // apply state changes here
	gasPool     *core.GasPool          // available gas used to pack transactions
	txs         []types.Transaction
	receipts    []*types.Receipt
	gasUsed     *uint64
	blobGasUsed *uint64
}

func (env *environment) clone() *environment {
	return &environment{
		signer:        env.signer,
		engine:        env.engine,
		chainConfig:   env.chainConfig,
		vmConfig:      env.vmConfig,
		verifier:      env.verifier,
		getHeaderFunc: env.getHeaderFunc,
		block:         env.block,
		txpool:        env.txpool,
		txpoolTx:      env.txpoolTx,
		state:         env.state.Copy(),
		gasPool:       new(core.GasPool).AddGas(env.gasPool.Gas()),
		txs:           env.txs[:],
		receipts:      env.receipts[:],
		gasUsed:       &(*env.gasUsed),
		blobGasUsed:   &(*env.blobGasUsed),
	}
}

func (env *environment) commitTransaction(tx types.Transaction) error {
	env.state.SetTxContext(tx.Hash(), common.Hash{}, len(env.txs))

	snap := env.state.Snapshot()
	gasSnap := env.gasPool.Gas()

	receipt, err := env.executeTransaction(tx)

	if err != nil {
		env.state.RevertToSnapshot(snap)
		env.gasPool = new(core.GasPool).AddGas(gasSnap) // restore gasPool as well as state
		return err
	}

	env.txs = append(env.txs, tx)
	env.receipts = append(env.receipts, receipt)
	return nil
}

// tx is finalized if error == nil
func (env *environment) executeTransaction(tx types.Transaction) (*types.Receipt, error) {
	vmConfig := &(*env.vmConfig)
	var tracer *logger.AccessListTracer = nil
	if env.verifier != nil {
		precompiles := vm.ActivePrecompiles(env.chainConfig.Rules(env.block.Header.Number.Uint64(), env.block.Header.Time))
		excludes := make(map[common.Address]struct{})
		for _, precompile := range precompiles {
			excludes[precompile] = struct{}{}
		}

		tracer = logger.NewAccessListTracer(nil, excludes, env.state)
		vmConfig.Debug = true
		vmConfig.Tracer = tracer
	}

	receipt, _, err := core.ApplyTransactionWithValidation(
		env.chainConfig,
		core.GetHashFn(env.block.Header, env.getHeaderFunc),
		env.engine,
		&env.block.Header.Coinbase,
		env.gasPool,
		env.state,
		state.NewNoopWriter(),
		env.block.Header,
		tx,
		env.gasUsed,
		env.blobGasUsed,
		*vmConfig,
		func(_ *core.ExecutionResult) error {
			if env.verifier == nil || tracer == nil {
				return nil
			}
			return env.verifier.verifyTrace(tracer)
		},
	)
	return receipt, err
}

// commitBundle, compared to commitTransaction, is all-or-nothing mode of execution. "env" is updated only if
// the bundle executes in full.
func (env *environment) commitBundle(bundle *types.MevBundle) (*mevBundleSimulation, error) {
	execState := env.clone()

	sim, err := execState.executeBundle(bundle)
	if err != nil {
		return nil, err
	}

	env.txs = append(env.txs, bundle.Txs...)
	env.receipts = append(env.receipts, sim.receipts...)

	*env = *execState
	return sim, nil
}

func (env *environment) executeBundle(bundle *types.MevBundle) (*mevBundleSimulation, error) {
	baseFee, _ := uint256.FromBig(env.block.Header.BaseFee)

	receipts := make([]*types.Receipt, len(bundle.Txs))
	totalSentToCoinbase := big.NewInt(0)
	totalGasFees := big.NewInt(0)
	totalGasUsed := uint64(0)
	for txnIndex, txn := range bundle.Txs {
		coinbaseBalanceStart := env.state.GetBalance(env.block.Header.Coinbase).ToBig()

		env.state.SetTxContext(txn.Hash(), common.Hash{}, len(env.txs)+txnIndex)

		receipt, err := env.executeTransaction(txn)
		if err != nil {
			return nil, err
		}
		if receipt.Status == types.ReceiptStatusFailed && !containsHash(bundle.RevertingTxHashes, txn.Hash()) {
			return nil, errors.New("tx failed")
		}

		gasUsed := new(big.Int).SetUint64(receipt.GasUsed)
		gasPrice := txn.GetEffectiveGasTip(baseFee).ToBig()

		gasFees := gasUsed.Mul(gasUsed, gasPrice)
		coinbaseBalanceEnd := env.state.GetBalance(env.block.Header.Coinbase).ToBig()
		coinbaseDelta := coinbaseBalanceStart.Sub(coinbaseBalanceStart, coinbaseBalanceEnd)

		totalSentToCoinbase.Add(totalSentToCoinbase, coinbaseDelta)
		totalSentToCoinbase.Sub(totalSentToCoinbase, gasFees)
		totalGasUsed += receipt.GasUsed
		receipts = append(receipts, receipt)

		isPublicTx, err := env.txpool.IdHashKnown(env.txpoolTx, txn.Hash().Bytes())
		if err != nil {
			return nil, err
		}

		if !isPublicTx {
			totalGasFees.Add(totalGasFees, gasFees)
		}
	}

	totalEth := new(big.Int).Add(totalSentToCoinbase, totalGasFees)
	bundleGasPrice := new(big.Int).Div(totalEth, new(big.Int).SetUint64(totalGasUsed))
	return &mevBundleSimulation{
		bundle:            bundle,
		receipts:          receipts,
		mevGasPrice:       bundleGasPrice,
		totalEth:          totalEth,
		ethSentToCoinbase: totalSentToCoinbase,
		totalGasUsed:      totalGasUsed,
	}, nil
}

func containsHash(arr []common.Hash, match common.Hash) bool {
	for _, elem := range arr {
		if elem == match {
			return true
		}
	}
	return false
}
