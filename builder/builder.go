package builder

import (
	"crypto/ecdsa"
	"errors"
	capellaapi "github.com/attestantio/go-builder-client/api/capella"
	v1 "github.com/attestantio/go-builder-client/api/v1"
	"github.com/attestantio/go-eth2-client/spec/bellatrix"
	"github.com/attestantio/go-eth2-client/spec/capella"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/flashbots/go-boost-utils/ssz"
	"github.com/flashbots/go-boost-utils/utils"
	"github.com/holiman/uint256"
	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/common/hexutility"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/crypto"
	"github.com/ledgerwatch/erigon/turbo/engineapi/engine_types"
	"github.com/ledgerwatch/log/v3"
	"golang.org/x/time/rate"
	"math/big"
	_ "os"
	"strings"
	"time"

	"github.com/flashbots/go-boost-utils/bls"
	boostTypes "github.com/flashbots/go-boost-utils/types"
)

const (
	RateLimitIntervalDefault                    = 500 * time.Millisecond
	RateLimitBurstDefault                       = 10
	BlockResubmitIntervalDefault                = 500 * time.Millisecond
	SubmissionOffsetFromEndOfSlotSecondsDefault = 3 * time.Second
)

type PubkeyHex string

type ValidatorData struct {
	Pubkey       PubkeyHex
	FeeRecipient bellatrix.ExecutionAddress `json:"feeRecipient"`
	GasLimit     uint64                     `json:"gasLimit"`
}

type BuilderParams struct {
	FeeRecipient   common.Address
	FeeRecipientPk *ecdsa.PrivateKey
	PaymentPercent *big.Int
	Verifier       *AccessVerifier
}

type IRelay interface {
	SubmitBlock(msg *capellaapi.SubmitBlockRequest, vd ValidatorData) error
	GetValidatorForSlot(nextSlot uint64) (ValidatorData, error)
	Start() error
}

type IBuilder interface {
	OnPayloadAttribute(attrs *BuilderPayloadAttributes) error
	Start() error
}

type Builder struct {
	beaconClient                IBeaconClient
	relay                       IRelay
	eth                         IEthereumService
	resubmitter                 Resubmitter
	params                      *BuilderParams
	verifier                    *AccessVerifier
	dryRun                      bool
	ignoreLatePayloadAttributes bool

	builderSecretKey     *bls.SecretKey
	builderPublicKey     phase0.BLSPubKey
	builderSigningDomain phase0.Domain

	bestBid *types.Block
}

// BuilderArgs is a struct that contains all the arguments needed to create a new Builder
type BuilderArgs struct {
	feeRecipientPk                string
	sk                            *bls.SecretKey
	relay                         IRelay
	builderSigningDomain          phase0.Domain
	builderBlockResubmitInterval  time.Duration
	discardRevertibleTxOnErr      bool
	eth                           IEthereumService
	dryRun                        bool
	ignoreLatePayloadAttributes   bool
	beaconClient                  IBeaconClient
	submissionOffsetFromEndOfSlot time.Duration
	blacklistFile                 string

	limiter *rate.Limiter
}

func NewBuilder(args BuilderArgs) *Builder {
	feeRecipientPk, err := crypto.HexToECDSA(strings.TrimPrefix(args.feeRecipientPk, "0x"))
	if err != nil {
		panic("incorrect fee recipient private key")
	}

	publicKey := feeRecipientPk.Public()
	publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
	var feeRecipientAddress common.Address
	if ok {
		feeRecipientAddress = crypto.PubkeyToAddress(*publicKeyECDSA)
	} else {
		panic("Cannot assert type, builder fee recipient private key")
	}

	pkBytes, err := bls.PublicKeyFromSecretKey(args.sk)
	if err != nil {
		panic("Error getting public key from secret key")
	}
	pk, err := utils.BlsPublicKeyToPublicKey(pkBytes)
	if err != nil {
		panic("Error converting bls public key to public key")
	}
	log.Info("Builder Pubkey", "pubkey", pk.String())
	log.Info("Builder Signing Domain", "domain", hexutility.Encode(args.builderSigningDomain[:]))

	var verifier *AccessVerifier = nil
	if args.blacklistFile != "" {
		verifier, _ = NewAccessVerifierFromFile(args.blacklistFile)
	}

	return &Builder{
		beaconClient: args.beaconClient,
		relay:        args.relay,
		eth:          args.eth,
		resubmitter:  Resubmitter{},
		params: &BuilderParams{
			FeeRecipient:   feeRecipientAddress,
			FeeRecipientPk: feeRecipientPk,
			PaymentPercent: big.NewInt(100),
			Verifier:       verifier,
		},

		builderSecretKey:     args.sk,
		builderPublicKey:     pk,
		builderSigningDomain: args.builderSigningDomain,
	}
}

func (b *Builder) Start() error {
	// Start regular payload attributes updates
	go func() {
		c := make(chan BuilderPayloadAttributes)
		go b.beaconClient.SubscribeToPayloadAttributesEvents(c)

		currentSlot := uint64(0)

		for {
			select {
			case payloadAttributes := <-c:
				// Right now we are building only on a single head. This might change in the future!
				if payloadAttributes.Slot < currentSlot {
					continue
				} else if payloadAttributes.Slot == currentSlot {
					// Subsequent sse events should only be canonical!
					if !b.ignoreLatePayloadAttributes {
						err := b.OnPayloadAttribute(&payloadAttributes)
						if err != nil {
							log.Error("error with builder processing on payload attribute",
								"latestSlot", currentSlot,
								"processedSlot", payloadAttributes.Slot,
								"headHash", payloadAttributes.HeadHash.String(),
								"error", err)
						}
					}
				} else if payloadAttributes.Slot > currentSlot {
					currentSlot = payloadAttributes.Slot
					err := b.OnPayloadAttribute(&payloadAttributes)
					if err != nil {
						log.Error("error with builder processing on payload attribute",
							"latestSlot", currentSlot,
							"processedSlot", payloadAttributes.Slot,
							"headHash", payloadAttributes.HeadHash.String(),
							"error", err)
					}
				}
			}
		}
	}()

	return b.relay.Start()
}

func (b *Builder) onSealedBlock(executableData *engine_types.ExecutionPayload, block *types.Block, proposerPubkey phase0.BLSPubKey, vd ValidatorData, slot uint64) error {
	bestBid := b.bestBid
	if bestBid != nil && block.ParentHash() == bestBid.ParentHash() && block.Number().Cmp(bestBid.Number()) == 0 && block.Profit.Cmp(bestBid.Profit) <= 0 {
		log.Info("Skip resubmitting same/worse bid for the same slot", "slot", slot, "profit_best", bestBid.Profit, "profit_current", block.Profit)
		return nil
	}

	payload, err := executableDataToExecutionPayload(executableData)
	if err != nil {
		log.Error("could not format execution payload", "err", err)
		return err
	}

	if block.Profit.Sign() < 1 {
		log.Error("block value not > 0")
		return errors.New("block value not > 0")
	}

	value, overflow := uint256.FromBig(block.Profit)
	if overflow {
		log.Error("could not set block value due to value overflow")
		return err
	}

	blockBidMsg := v1.BidTrace{
		Slot:                 slot,
		ParentHash:           payload.ParentHash,
		BlockHash:            payload.BlockHash,
		BuilderPubkey:        b.builderPublicKey,
		ProposerPubkey:       proposerPubkey,
		ProposerFeeRecipient: vd.FeeRecipient,
		GasLimit:             uint64(executableData.GasLimit),
		GasUsed:              uint64(executableData.GasUsed),
		Value:                value,
	}

	signature, err := ssz.SignMessage(&blockBidMsg, b.builderSigningDomain, b.builderSecretKey)
	if err != nil {
		log.Error("could not sign builder bid", "err", err)
		return err
	}

	blockSubmitReq := capellaapi.SubmitBlockRequest{
		Signature:        signature,
		Message:          &blockBidMsg,
		ExecutionPayload: payload,
	}
	log.Info("Submitting new block to relay", "slot", slot, "number", block.NumberU64(), "txs", block.Transactions().Len(), "profit", block.Profit)

	err = b.relay.SubmitBlock(&blockSubmitReq, vd)
	if err != nil {
		log.Error("could not submit block", "err", err)
		return err
	}

	b.bestBid = block

	return nil
}

func (b *Builder) OnPayloadAttribute(attrs *BuilderPayloadAttributes) error {
	if attrs == nil {
		return nil
	}

	vd, err := b.relay.GetValidatorForSlot(attrs.Slot)
	if err != nil {
		log.Info("could not get validator while submitting block", "err", err, "slot", attrs.Slot)
		return err
	}

	attrs.SuggestedFeeRecipient = [20]byte(vd.FeeRecipient)
	attrs.GasLimit = vd.GasLimit

	proposerPubkey, err := utils.HexToPubkey(string(vd.Pubkey))
	if err != nil {
		log.Error("could not parse pubkey", "err", err, "pubkey", vd.Pubkey)
		return err
	}

	if !b.eth.Synced() {
		return errors.New("backend not Synced")
	}

	parentBlock := b.eth.GetBlockByHash(attrs.HeadHash)
	if parentBlock == nil {
		log.Info("Block hash not found in blocktree", "head block hash", attrs.HeadHash)
		return errors.New("parent block not found in blocktree")
	}

	firstBlockResult := b.resubmitter.newContinuousTask(12*time.Second, func() error {
		executableData, block := b.eth.BuildBlock(attrs, b.params, nil)
		if executableData == nil || block == nil {
			log.Error("did not receive the payload")
			return errors.New("did not receive the payload")
		}

		err := b.onSealedBlock(executableData, block, proposerPubkey, vd, attrs.Slot)
		if err != nil {
			log.Error("could not run block hook", "err", err)
			return err
		}

		return nil
	})

	return firstBlockResult
}

func executableDataToExecutionPayload(data *engine_types.ExecutionPayload) (*capella.ExecutionPayload, error) {
	transactionData := make([]bellatrix.Transaction, len(data.Transactions))
	for i, tx := range data.Transactions {
		transactionData[i] = bellatrix.Transaction(tx)
	}

	withdrawalData := make([]*capella.Withdrawal, len(data.Withdrawals))
	for i, wd := range data.Withdrawals {
		withdrawalData[i] = &capella.Withdrawal{
			Index:          capella.WithdrawalIndex(wd.Index),
			ValidatorIndex: phase0.ValidatorIndex(wd.Validator),
			Address:        bellatrix.ExecutionAddress(wd.Address),
			Amount:         phase0.Gwei(wd.Amount),
		}
	}

	baseFeePerGas := new(boostTypes.U256Str)
	err := baseFeePerGas.FromBig((*big.Int)(data.BaseFeePerGas))
	if err != nil {
		return nil, err
	}

	return &capella.ExecutionPayload{
		ParentHash:    [32]byte(data.ParentHash),
		FeeRecipient:  [20]byte(data.FeeRecipient),
		StateRoot:     data.StateRoot,
		ReceiptsRoot:  data.ReceiptsRoot,
		LogsBloom:     types.BytesToBloom(data.LogsBloom),
		PrevRandao:    data.PrevRandao,
		BlockNumber:   uint64(data.BlockNumber),
		GasLimit:      uint64(data.GasLimit),
		GasUsed:       uint64(data.GasUsed),
		Timestamp:     uint64(data.Timestamp),
		ExtraData:     data.ExtraData,
		BaseFeePerGas: *baseFeePerGas,
		BlockHash:     [32]byte(data.BlockHash),
		Transactions:  transactionData,
		Withdrawals:   withdrawalData,
	}, nil
}
