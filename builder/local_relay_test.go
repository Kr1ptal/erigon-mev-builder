package builder

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/attestantio/go-builder-client/api"
	capellaapi "github.com/attestantio/go-builder-client/api/capella"
	v1 "github.com/attestantio/go-builder-client/api/v1"
	"github.com/attestantio/go-builder-client/spec"
	consensusapiv1capella "github.com/attestantio/go-eth2-client/api/v1/capella"
	"github.com/attestantio/go-eth2-client/spec/altair"
	"github.com/attestantio/go-eth2-client/spec/bellatrix"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/flashbots/go-boost-utils/ssz"
	"github.com/holiman/uint256"
	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/common/hexutil"
	"github.com/ledgerwatch/erigon-lib/common/hexutility"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/turbo/engineapi/engine_types"
	"github.com/ledgerwatch/log/v3"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/flashbots/go-boost-utils/bls"
	"github.com/stretchr/testify/require"
)

func newTestBackend(t *testing.T, forkchoiceData *engine_types.ExecutionPayload, block *types.Block) (*Builder, *LocalRelay, *ValidatorPrivateData) {
	validator := NewRandomValidator()
	sk, _ := bls.GenerateRandomSecretKey()
	bDomain := ssz.ComputeDomain(ssz.DomainTypeAppBuilder, [4]byte{0x02, 0x0, 0x0, 0x0}, phase0.Root{})
	genesisValidatorsRoot := phase0.Root(common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000000"))
	cDomain := ssz.ComputeDomain(ssz.DomainTypeBeaconProposer, [4]byte{0x02, 0x0, 0x0, 0x0}, genesisValidatorsRoot)
	beaconClient := &testBeaconClient{validator: validator}
	localRelay := NewLocalRelay(sk, beaconClient, bDomain, cDomain, ForkData{}, true)
	ethService := &testEthereumService{synced: true, testExecutableData: forkchoiceData, testBlock: block, testTxPool: &testTxPool{}}

	builderArgs := BuilderArgs{
		feeRecipientPk:                "0xb71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291",
		sk:                            sk,
		relay:                         localRelay,
		builderSigningDomain:          bDomain,
		builderBlockResubmitInterval:  0,
		discardRevertibleTxOnErr:      false,
		eth:                           ethService,
		dryRun:                        false,
		ignoreLatePayloadAttributes:   false,
		beaconClient:                  beaconClient,
		submissionOffsetFromEndOfSlot: 0,
		blacklistFile:                 "",
		limiter:                       nil,
	}
	backend := NewBuilder(builderArgs)
	// service := NewService("127.0.0.1:31545", backend)

	return backend, localRelay, validator
}

func testRequest(t *testing.T, localRelay *LocalRelay, method string, path string, payload any) *httptest.ResponseRecorder {
	var req *http.Request
	var err error

	if payload == nil {
		req, err = http.NewRequest(method, path, nil)
	} else {
		payloadBytes, err2 := json.Marshal(payload)
		require.NoError(t, err2)
		req, err = http.NewRequest(method, path, bytes.NewReader(payloadBytes))
	}

	require.NoError(t, err)
	rr := httptest.NewRecorder()
	getRouter(localRelay).ServeHTTP(rr, req)
	return rr
}

func TestValidatorRegistration(t *testing.T) {
	_, relay, _ := newTestBackend(t, nil, nil)
	log.Error("rsk", "sk", hexutility.Encode(relay.relaySecretKey.Marshal()))

	v := NewRandomValidator()
	payload, err := prepareRegistrationMessage(t, relay.builderSigningDomain, v)
	require.NoError(t, err)

	rr := testRequest(t, relay, "POST", "/eth/v1/builder/validators", payload)
	require.Equal(t, http.StatusOK, rr.Code)
	require.Contains(t, relay.validators, PubkeyHex(v.Pk.String()))
	require.Equal(t, FullValidatorData{ValidatorData: ValidatorData{Pubkey: PubkeyHex(v.Pk.String()), FeeRecipient: payload[0].Message.FeeRecipient, GasLimit: payload[0].Message.GasLimit}, Timestamp: uint64(payload[0].Message.Timestamp.Unix())}, relay.validators[PubkeyHex(v.Pk.String())])

	rr = testRequest(t, relay, "POST", "/eth/v1/builder/validators", payload)
	require.Equal(t, http.StatusOK, rr.Code)

	payload[0].Message.Timestamp = payload[0].Message.Timestamp.Add(time.Second)
	// Invalid signature
	payload[0].Signature[len(payload[0].Signature)-1] = 0x00
	rr = testRequest(t, relay, "POST", "/eth/v1/builder/validators", payload)
	require.Equal(t, http.StatusBadRequest, rr.Code)
	require.Equal(t, `{"code":400,"message":"invalid signature"}`+"\n", rr.Body.String())

	// TODO: cover all errors
}

func prepareRegistrationMessage(t *testing.T, domain phase0.Domain, v *ValidatorPrivateData) ([]v1.SignedValidatorRegistration, error) {
	var pubkey phase0.BLSPubKey
	copy(pubkey[:], v.Pk)
	require.Equal(t, []byte(v.Pk), pubkey[:])

	msg := v1.ValidatorRegistration{
		FeeRecipient: bellatrix.ExecutionAddress{0x42},
		GasLimit:     15_000_000,
		Timestamp:    time.Now(),
		Pubkey:       pubkey,
	}

	signature, err := v.Sign(&msg, domain)
	require.NoError(t, err)

	return []v1.SignedValidatorRegistration{{
		Message:   &msg,
		Signature: signature,
	}}, nil
}

func registerValidator(t *testing.T, v *ValidatorPrivateData, relay *LocalRelay) {
	payload, err := prepareRegistrationMessage(t, relay.builderSigningDomain, v)
	require.NoError(t, err)

	log.Info("Registering", "payload", payload[0].Message)
	rr := testRequest(t, relay, "POST", "/eth/v1/builder/validators", payload)
	require.Equal(t, http.StatusOK, rr.Code)
	require.Contains(t, relay.validators, PubkeyHex(v.Pk.String()))
	require.Equal(t, FullValidatorData{ValidatorData: ValidatorData{Pubkey: PubkeyHex(v.Pk.String()), FeeRecipient: payload[0].Message.FeeRecipient, GasLimit: payload[0].Message.GasLimit}, Timestamp: uint64(payload[0].Message.Timestamp.Unix())}, relay.validators[PubkeyHex(v.Pk.String())])
}

func TestGetHeader(t *testing.T) {
	baseFeePerGas := hexutil.Big(*big.NewInt(12))
	forkchoiceData := &engine_types.ExecutionPayload{
		ParentHash:    common.HexToHash("0xafafafa"),
		FeeRecipient:  common.Address{0x01},
		BlockHash:     common.HexToHash("0xbfbfbfb"),
		BaseFeePerGas: &baseFeePerGas,
		ExtraData:     []byte{},
		LogsBloom:     []byte{0x00, 0x05, 0x10},
	}
	forkchoiceBlock := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(1)})
	forkchoiceBlock.Profit = big.NewInt(10)

	backend, relay, validator := newTestBackend(t, forkchoiceData, forkchoiceBlock)

	path := fmt.Sprintf("/eth/v1/builder/header/%d/%s/%s", 0, forkchoiceData.ParentHash.Hex(), validator.Pk.String())
	rr := testRequest(t, relay, "GET", path, nil)
	require.Equal(t, `{"code":400,"message":"unknown validator"}`+"\n", rr.Body.String())

	registerValidator(t, validator, relay)

	rr = testRequest(t, relay, "GET", path, nil)
	require.Equal(t, `{"code":400,"message":"unknown payload"}`+"\n", rr.Body.String())

	path = fmt.Sprintf("/eth/v1/builder/header/%d/%s/%s", 0, forkchoiceData.ParentHash.Hex(), NewRandomValidator().Pk.String())
	rr = testRequest(t, relay, "GET", path, nil)
	require.Equal(t, ``, rr.Body.String())
	require.Equal(t, 204, rr.Code)

	backend.OnPayloadAttribute(&BuilderPayloadAttributes{})

	path = fmt.Sprintf("/eth/v1/builder/header/%d/%s/%s", 0, forkchoiceData.ParentHash.Hex(), validator.Pk.String())
	rr = testRequest(t, relay, "GET", path, nil)
	require.Equal(t, http.StatusOK, rr.Code)

	bid := new(spec.VersionedSignedBuilderBid)
	err := json.Unmarshal(rr.Body.Bytes(), bid)
	require.NoError(t, err)

	executionPayload, err := executableDataToExecutionPayload(forkchoiceData)
	require.NoError(t, err)
	expectedHeader, err := PayloadToPayloadHeader(executionPayload)
	require.NoError(t, err)
	expectedValue, ok := uint256.FromBig(big.NewInt(10))
	require.False(t, ok)
	require.EqualValues(t, &capellaapi.BuilderBid{
		Header: expectedHeader,
		Value:  expectedValue,
		Pubkey: backend.builderPublicKey,
	}, bid.Capella.Message)

	require.Equal(t, forkchoiceData.ParentHash.Bytes(), bid.Capella.Message.Header.ParentHash[:], "didn't build on expected parent")
	ok, err = ssz.VerifySignature(bid.Capella.Message, backend.builderSigningDomain, backend.builderPublicKey[:], bid.Capella.Signature[:])

	require.NoError(t, err)
	require.True(t, ok)
	backend.resubmitter.cancel()
}

func TestGetPayload(t *testing.T) {
	baseFeePerGas := hexutil.Big(*big.NewInt(12))
	forkchoiceData := &engine_types.ExecutionPayload{
		ParentHash:    common.HexToHash("0xafafafa"),
		FeeRecipient:  common.Address{0x01},
		BlockHash:     common.HexToHash("0xbfbfbfb"),
		BaseFeePerGas: &baseFeePerGas,
		ExtraData:     []byte{},
	}
	forkchoiceBlock := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(1)})
	forkchoiceBlock.Profit = big.NewInt(10)

	backend, relay, validator := newTestBackend(t, forkchoiceData, forkchoiceBlock)

	registerValidator(t, validator, relay)
	backend.OnPayloadAttribute(&BuilderPayloadAttributes{})

	path := fmt.Sprintf("/eth/v1/builder/header/%d/%s/%s", 0, forkchoiceData.ParentHash.Hex(), validator.Pk.String())
	rr := testRequest(t, relay, "GET", path, nil)
	require.Equal(t, http.StatusOK, rr.Code)

	bid := new(spec.VersionedSignedBuilderBid)
	err := json.Unmarshal(rr.Body.Bytes(), bid)
	require.NoError(t, err)

	blockHash := [32]byte{0x06}
	syncCommitteeBits := [64]byte{0x07}

	// Create request payload
	msg := &consensusapiv1capella.BlindedBeaconBlock{
		Slot:          1,
		ProposerIndex: 2,
		ParentRoot:    phase0.Root{0x03},
		StateRoot:     phase0.Root{0x04},
		Body: &consensusapiv1capella.BlindedBeaconBlockBody{
			ETH1Data: &phase0.ETH1Data{
				DepositRoot:  phase0.Root{0x05},
				DepositCount: 5,
				BlockHash:    blockHash[:],
			},
			ProposerSlashings: []*phase0.ProposerSlashing{},
			AttesterSlashings: []*phase0.AttesterSlashing{},
			Attestations:      []*phase0.Attestation{},
			Deposits:          []*phase0.Deposit{},
			VoluntaryExits:    []*phase0.SignedVoluntaryExit{},
			SyncAggregate: &altair.SyncAggregate{
				SyncCommitteeBits:      syncCommitteeBits[:],
				SyncCommitteeSignature: phase0.BLSSignature{0x08},
			},
			ExecutionPayloadHeader: bid.Capella.Message.Header,
		},
	}

	// TODO: test wrong signing domain
	signature, err := validator.Sign(msg, relay.proposerSigningDomain)
	require.NoError(t, err)

	// Call getPayload with invalid signature
	rr = testRequest(t, relay, "POST", "/eth/v1/builder/blinded_blocks", &consensusapiv1capella.SignedBlindedBeaconBlock{
		Message:   msg,
		Signature: phase0.BLSSignature{0x09},
	})
	require.Equal(t, http.StatusBadRequest, rr.Code)
	require.Equal(t, `{"code":400,"message":"invalid signature"}`+"\n", rr.Body.String())

	// Call getPayload with correct signature
	rr = testRequest(t, relay, "POST", "/eth/v1/builder/blinded_blocks", &consensusapiv1capella.SignedBlindedBeaconBlock{
		Message:   msg,
		Signature: signature,
	})

	// Verify getPayload response
	require.Equal(t, http.StatusOK, rr.Code)
	getPayloadResponse := new(api.VersionedExecutionPayload)
	err = json.Unmarshal(rr.Body.Bytes(), getPayloadResponse)
	require.NoError(t, err)
	require.Equal(t, bid.Capella.Message.Header.BlockHash, getPayloadResponse.Capella.BlockHash)

	backend.resubmitter.cancel()
}
