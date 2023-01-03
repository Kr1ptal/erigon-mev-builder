package mev

import (
	"context"
	"errors"
	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/common/hexutility"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/rpc"
	"math/big"
)

// SendBundleArgs represents the arguments for a SendBundle call.
type SendBundleArgs struct {
	Txs               []hexutility.Bytes `json:"txs"`
	BlockNumber       rpc.BlockNumber    `json:"blockNumber"`
	MinTimestamp      *uint64            `json:"minTimestamp"`
	MaxTimestamp      *uint64            `json:"maxTimestamp"`
	RevertingTxHashes []common.Hash      `json:"revertingTxHashes"`
}

type BundlePoolApi struct {
	pool *BundlePool
}

func NewBundlePoolApi(pool *BundlePool) *BundlePoolApi {
	return &BundlePoolApi{pool: pool}
}

func (b *BundlePoolApi) SendBundle(_ context.Context, args SendBundleArgs) error {
	var txs types.Transactions
	if len(args.Txs) == 0 {
		return errors.New("bundle missing txs")
	}
	if args.BlockNumber == 0 {
		return errors.New("bundle missing blockNumber")
	}

	for _, encodedTx := range args.Txs {
		tx, err := types.DecodeTransaction(encodedTx)

		if err != nil {
			return err
		}

		txs = append(txs, tx)
	}

	var minTimestamp, maxTimestamp uint64
	if args.MinTimestamp != nil {
		minTimestamp = *args.MinTimestamp
	}
	if args.MaxTimestamp != nil {
		maxTimestamp = *args.MaxTimestamp
	}

	b.pool.AddMevBundle(&types.MevBundle{
		Txs:               txs,
		BlockNumber:       big.NewInt(args.BlockNumber.Int64()),
		MinTimestamp:      minTimestamp,
		MaxTimestamp:      maxTimestamp,
		RevertingTxHashes: args.RevertingTxHashes,
	})
	return nil
}
