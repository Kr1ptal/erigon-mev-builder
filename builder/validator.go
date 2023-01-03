package builder

import (
	"errors"
	v1 "github.com/attestantio/go-builder-client/api/v1"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/flashbots/go-boost-utils/ssz"
	"github.com/flashbots/go-boost-utils/utils"
	"github.com/ledgerwatch/erigon-lib/common/hexutility"
	"time"

	"github.com/flashbots/go-boost-utils/bls"
)

type ValidatorPrivateData struct {
	sk *bls.SecretKey
	Pk hexutility.Bytes
}

func NewRandomValidator() *ValidatorPrivateData {
	sk, pk, err := bls.GenerateNewKeypair()
	if err != nil {
		return nil
	}
	return &ValidatorPrivateData{sk, bls.PublicKeyToBytes(pk)}
}

func (v *ValidatorPrivateData) Sign(msg ssz.ObjWithHashTreeRoot, d phase0.Domain) (phase0.BLSSignature, error) {
	return ssz.SignMessage(msg, d, v.sk)
}

func (v *ValidatorPrivateData) PrepareRegistrationMessage(feeRecipientHex string) (v1.SignedValidatorRegistration, error) {
	address, err := utils.HexToAddress(feeRecipientHex)
	if err != nil {
		return v1.SignedValidatorRegistration{}, err
	}

	if len(v.Pk) != 48 {
		return v1.SignedValidatorRegistration{}, errors.New("invalid public key")
	}

	pubkey := phase0.BLSPubKey{}
	copy(pubkey[:], v.Pk)

	msg := &v1.ValidatorRegistration{
		FeeRecipient: address,
		GasLimit:     1000,
		Timestamp:    time.Now(),
		Pubkey:       pubkey,
	}
	signature, err := v.Sign(msg, ssz.DomainBuilder)
	if err != nil {
		return v1.SignedValidatorRegistration{}, err
	}
	return v1.SignedValidatorRegistration{Message: msg, Signature: signature}, nil
}
