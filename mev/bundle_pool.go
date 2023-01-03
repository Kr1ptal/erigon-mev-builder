package mev

import (
	"github.com/ledgerwatch/erigon/core/types"
	"math/big"
	"sync"
)

type BundlePool struct {
	mu      sync.Mutex
	bundles []*types.MevBundle
}

func NewBundlePool() *BundlePool {
	return &BundlePool{}
}

func (pool *BundlePool) AddMevBundle(bundle *types.MevBundle) {
	pool.mu.Lock()
	pool.bundles = append(pool.bundles, bundle)
	pool.mu.Unlock()
}

func (pool *BundlePool) MevBundles(blockNumber *big.Int, blockTimestamp uint64) []*types.MevBundle {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	// returned values
	var ret []*types.MevBundle
	// rolled over values
	var bundles []*types.MevBundle

	for _, bundle := range pool.bundles {
		// Prune outdated bundles
		if (bundle.MaxTimestamp != 0 && blockTimestamp > bundle.MaxTimestamp) || blockNumber.Cmp(bundle.BlockNumber) > 0 {
			continue
		}

		// Roll over future bundles
		if (bundle.MinTimestamp != 0 && blockTimestamp < bundle.MinTimestamp) || blockNumber.Cmp(bundle.BlockNumber) < 0 {
			bundles = append(bundles, bundle)
			continue
		}

		// return the ones which are in time
		ret = append(ret, bundle)
		// keep the bundles around internally until they need to be pruned
		bundles = append(bundles, bundle)
	}

	pool.bundles = bundles
	return ret
}
