package builder

import (
	"context"
	"github.com/ledgerwatch/erigon/eth/stagedsync"
	"github.com/ledgerwatch/log/v3"

	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon/eth/stagedsync/stages"
)

func BuilderStages(
	ctx context.Context,
	createBlockCfg stagedsync.MiningCreateBlockCfg,
	execCfg BuilderExecConfig,
	hashStateCfg stagedsync.HashStateCfg,
	trieCfg stagedsync.TrieCfg,
	finish stagedsync.MiningFinishCfg,
) []*stagedsync.Stage {
	return []*stagedsync.Stage{
		{
			ID:          stages.MiningCreateBlock,
			Description: "Mining: construct new block from tx pool",
			Forward: func(firstCycle bool, badBlockUnwind bool, s *stagedsync.StageState, u stagedsync.Unwinder, tx kv.RwTx, logger log.Logger) error {
				return stagedsync.SpawnMiningCreateBlockStage(s, tx, createBlockCfg, ctx.Done(), logger)
			},
			Unwind: func(firstCycle bool, u *stagedsync.UnwindState, s *stagedsync.StageState, tx kv.RwTx, logger log.Logger) error {
				return nil
			},
			Prune: func(firstCycle bool, u *stagedsync.PruneState, tx kv.RwTx, logger log.Logger) error { return nil },
		},
		{
			ID:          stages.MiningExecution,
			Description: "Mining: construct new block as external builder",
			Forward: func(firstCycle bool, badBlockUnwind bool, s *stagedsync.StageState, u stagedsync.Unwinder, tx kv.RwTx, logger log.Logger) error {
				return SpawnBuilderExecStage(s, tx, execCfg, ctx.Done(), logger)
			},
			Unwind: func(firstCycle bool, u *stagedsync.UnwindState, s *stagedsync.StageState, tx kv.RwTx, logger log.Logger) error {
				return nil
			},
			Prune: func(firstCycle bool, u *stagedsync.PruneState, tx kv.RwTx, logger log.Logger) error { return nil },
		},
		{
			ID:          stages.HashState,
			Description: "Hash the key in the state",
			Forward: func(firstCycle bool, badBlockUnwind bool, s *stagedsync.StageState, u stagedsync.Unwinder, tx kv.RwTx, logger log.Logger) error {
				return stagedsync.SpawnHashStateStage(s, tx, hashStateCfg, ctx, logger)
			},
			Unwind: func(firstCycle bool, u *stagedsync.UnwindState, s *stagedsync.StageState, tx kv.RwTx, logger log.Logger) error {
				return nil
			},
			Prune: func(firstCycle bool, u *stagedsync.PruneState, tx kv.RwTx, logger log.Logger) error { return nil },
		},
		{
			ID:          stages.IntermediateHashes,
			Description: "Generate intermediate hashes and computing state root",
			Forward: func(firstCycle bool, badBlockUnwind bool, s *stagedsync.StageState, u stagedsync.Unwinder, tx kv.RwTx, logger log.Logger) error {
				stateRoot, err := stagedsync.SpawnIntermediateHashesStage(s, u, tx, trieCfg, ctx, logger)
				if err != nil {
					return err
				}
				createBlockCfg.GetMiner().MiningBlock.Header.Root = stateRoot
				return nil
			},
			Unwind: func(firstCycle bool, u *stagedsync.UnwindState, s *stagedsync.StageState, tx kv.RwTx, logger log.Logger) error {
				return nil
			},
			Prune: func(firstCycle bool, u *stagedsync.PruneState, tx kv.RwTx, logger log.Logger) error { return nil },
		},
		{
			ID:          stages.MiningFinish,
			Description: "Mining: create and propagate valid block",
			Forward: func(firstCycle bool, badBlockUnwind bool, s *stagedsync.StageState, u stagedsync.Unwinder, tx kv.RwTx, logger log.Logger) error {
				return stagedsync.SpawnMiningFinishStage(s, tx, finish, ctx.Done(), logger)
			},
			Unwind: func(firstCycle bool, u *stagedsync.UnwindState, s *stagedsync.StageState, tx kv.RwTx, logger log.Logger) error {
				return nil
			},
			Prune: func(firstCycle bool, u *stagedsync.PruneState, tx kv.RwTx, logger log.Logger) error { return nil },
		},
	}
}
