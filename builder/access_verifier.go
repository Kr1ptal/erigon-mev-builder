package builder

import (
	"encoding/json"
	"errors"
	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon/eth/tracers/logger"
	"os"
)

var errBlacklisted = errors.New("address is blacklisted")

type AccessVerifier struct {
	blacklistedAddresses map[common.Address]struct{}
}

type blacklistedAddresses []common.Address

func NewAccessVerifierFromFile(path string) (*AccessVerifier, error) {
	bytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var ba blacklistedAddresses
	if err := json.Unmarshal(bytes, &ba); err != nil {
		return nil, err
	}

	blacklistedAddresses := make(map[common.Address]struct{}, len(ba))
	for _, address := range ba {
		blacklistedAddresses[address] = struct{}{}
	}

	return &AccessVerifier{
		blacklistedAddresses: blacklistedAddresses,
	}, nil
}

func (a *AccessVerifier) verifyAddress(address common.Address) error {
	_, isBlacklisted := a.blacklistedAddresses[address]
	if isBlacklisted {
		return errBlacklisted
	}
	return nil
}

func (a *AccessVerifier) verifyTrace(trace *logger.AccessListTracer) error {
	for _, access := range trace.AccessList() {
		err := a.verifyAddress(access.Address)
		if err != nil {
			return err
		}
	}
	return nil
}
