package builder

import (
	v1 "github.com/attestantio/go-builder-client/api/v1"
	"github.com/attestantio/go-eth2-client/spec/bellatrix"
	"github.com/attestantio/go-eth2-client/spec/capella"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/flashbots/go-boost-utils/bls"
	"github.com/flashbots/go-boost-utils/ssz"
	boostTypes "github.com/flashbots/go-boost-utils/types"
	"github.com/flashbots/go-boost-utils/utils"
	"github.com/holiman/uint256"
	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/common/hexutil"
	"github.com/ledgerwatch/erigon-lib/common/hexutility"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/turbo/engineapi/engine_types"
	"github.com/stretchr/testify/require"
	"math/big"
	"testing"
)

func TestOnPayloadAttributes(t *testing.T) {
	vsk, err := bls.SecretKeyFromBytes(hexutil.MustDecode("0x370bb8c1a6e62b2882f6ec76762a67b39609002076b95aae5b023997cf9b2dc9"))
	require.NoError(t, err)
	validator := &ValidatorPrivateData{
		sk: vsk,
		Pk: hexutil.MustDecode("0xb67d2c11bcab8c4394fc2faa9601d0b99c7f4b37e14911101da7d97077917862eed4563203d34b91b5cf0aa44d6cfa05"),
	}

	testBeacon := testBeaconClient{
		validator: validator,
		slot:      56,
	}

	feeRecipient, _ := utils.HexToAddress("0xabcf8e0d4e9587369b2301d0790347320302cc00")
	testRelay := testRelay{
		validator: ValidatorData{
			Pubkey:       PubkeyHex(testBeacon.validator.Pk.String()),
			FeeRecipient: feeRecipient,
			GasLimit:     10,
		},
	}

	sk, err := bls.SecretKeyFromBytes(hexutil.MustDecode("0x31ee185dad1220a8c88ca5275e64cf5a5cb09cb621cb30df52c9bee8fbaaf8d7"))
	require.NoError(t, err)

	bDomain := ssz.ComputeDomain(ssz.DomainTypeAppBuilder, [4]byte{0x02, 0x0, 0x0, 0x0}, phase0.Root{})

	baseFeePerGas := hexutil.Big(*big.NewInt(16))
	testExecutableData := &engine_types.ExecutionPayload{
		ParentHash:   common.Hash{0x02, 0x03},
		FeeRecipient: common.Address(feeRecipient),
		StateRoot:    common.Hash{0x07, 0x16},
		ReceiptsRoot: common.Hash{0x08, 0x20},
		LogsBloom:    hexutil.MustDecode("0x000000000000000000000000000000"),
		BlockNumber:  hexutil.Uint64(10),
		GasLimit:     hexutil.Uint64(50),
		GasUsed:      hexutil.Uint64(100),
		Timestamp:    hexutil.Uint64(105),
		ExtraData:    hexutil.MustDecode("0x0042fafc"),

		BaseFeePerGas: &baseFeePerGas,

		BlockHash:    common.Hash{0x09, 0xff},
		Transactions: []hexutility.Bytes{},
	}

	testBlock := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(1)})
	testBlock.Profit = big.NewInt(10)

	testPayloadAttributes := &BuilderPayloadAttributes{
		Timestamp:             hexutil.Uint64(104),
		Random:                common.Hash{0x05, 0x10},
		SuggestedFeeRecipient: common.Address{0x04, 0x10},
		GasLimit:              uint64(21),
		Slot:                  uint64(25),
	}

	testEthService := &testEthereumService{synced: true, testExecutableData: testExecutableData, testBlock: testBlock, testTxPool: &testTxPool{}}

	builderArgs := BuilderArgs{
		feeRecipientPk:                "0xb71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291",
		sk:                            sk,
		relay:                         &testRelay,
		builderSigningDomain:          bDomain,
		builderBlockResubmitInterval:  0,
		discardRevertibleTxOnErr:      false,
		eth:                           testEthService,
		dryRun:                        false,
		ignoreLatePayloadAttributes:   false,
		beaconClient:                  &testBeacon,
		submissionOffsetFromEndOfSlot: 0,
		blacklistFile:                 "",
		limiter:                       nil,
	}
	builder := NewBuilder(builderArgs)

	builder.OnPayloadAttribute(testPayloadAttributes)

	require.NotNil(t, testRelay.submittedMsg)
	expectedProposerPubkey, err := utils.HexToPubkey(testBeacon.validator.Pk.String())
	require.NoError(t, err)

	expectedMessage := v1.BidTrace{
		Slot:                 uint64(25),
		ParentHash:           phase0.Hash32{0x02, 0x03},
		BlockHash:            phase0.Hash32{0x09, 0xff},
		BuilderPubkey:        builder.builderPublicKey,
		ProposerPubkey:       expectedProposerPubkey,
		ProposerFeeRecipient: feeRecipient,
		GasLimit:             uint64(50),
		GasUsed:              uint64(100),
		Value:                &uint256.Int{0x0a},
	}

	require.Equal(t, expectedMessage, *testRelay.submittedMsg.Message)

	expectedExecutionPayload := capella.ExecutionPayload{
		ParentHash:    [32]byte(testExecutableData.ParentHash),
		FeeRecipient:  feeRecipient,
		StateRoot:     [32]byte(testExecutableData.StateRoot),
		ReceiptsRoot:  [32]byte(testExecutableData.ReceiptsRoot),
		LogsBloom:     [256]byte{},
		PrevRandao:    [32]byte(testExecutableData.PrevRandao),
		BlockNumber:   uint64(testExecutableData.BlockNumber),
		GasLimit:      uint64(testExecutableData.GasLimit),
		GasUsed:       uint64(testExecutableData.GasUsed),
		Timestamp:     uint64(testExecutableData.Timestamp),
		ExtraData:     hexutil.MustDecode("0x0042fafc"),
		BaseFeePerGas: boostTypes.U256Str{0x10},
		BlockHash:     expectedMessage.BlockHash,
		Transactions:  []bellatrix.Transaction{},
		Withdrawals:   make([]*capella.Withdrawal, 0),
	}

	require.Equal(t, expectedExecutionPayload, *testRelay.submittedMsg.ExecutionPayload)

	expectedSignature, err := utils.HexToSignature("0xb086abc231a515559128122a6618ad316a76195ad39aa28195c9e8921b98561ca4fd12e2e1ea8d50d8e22f7e36d42ee1084fef26672beceda7650a87061e412d7742705077ac3af3ca1a1c3494eccb22fe7c234fd547a285ba699ff87f0e7759")

	require.NoError(t, err)
	require.Equal(t, expectedSignature, testRelay.submittedMsg.Signature)

	require.Equal(t, uint64(25), testRelay.requestedSlot)

	builder.resubmitter.cancel()
}
