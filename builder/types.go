package builder

import (
	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/common/hexutil"
	"github.com/ledgerwatch/erigon/core/types"
	"golang.org/x/exp/slices"
)

type BuilderPayloadAttributes struct {
	Timestamp             hexutil.Uint64    `json:"timestamp"`
	Random                common.Hash       `json:"prevRandao"`
	SuggestedFeeRecipient common.Address    `json:"suggestedFeeRecipient,omitempty"`
	Slot                  uint64            `json:"slot"`
	HeadHash              common.Hash       `json:"blockHash"`
	Withdrawals           types.Withdrawals `json:"withdrawals"`
	GasLimit              uint64
}

func (attrs *BuilderPayloadAttributes) Equal(other *BuilderPayloadAttributes) bool {
	if attrs.Timestamp != other.Timestamp ||
		attrs.Random != other.Random ||
		attrs.SuggestedFeeRecipient != other.SuggestedFeeRecipient ||
		attrs.Slot != other.Slot ||
		attrs.HeadHash != other.HeadHash ||
		attrs.GasLimit != other.GasLimit {
		return false
	}

	if !slices.Equal(attrs.Withdrawals, other.Withdrawals) {
		return false
	}
	return true
}
