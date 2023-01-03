package builder

import (
	"context"
	"errors"
	"fmt"
	capellaapi "github.com/attestantio/go-builder-client/api/capella"
	"github.com/attestantio/go-eth2-client/spec/bellatrix"
	"github.com/ledgerwatch/erigon-lib/common/hexutil"
	"github.com/ledgerwatch/log/v3"
	"net/http"
	"strings"
	"sync"
	"time"
)

type testRelay struct {
	validator     ValidatorData
	requestedSlot uint64
	submittedMsg  *capellaapi.SubmitBlockRequest
}

func (r *testRelay) Start() error {
	return nil
}

func (r *testRelay) SubmitBlock(msg *capellaapi.SubmitBlockRequest, _ ValidatorData) error {
	r.submittedMsg = msg
	return nil
}
func (r *testRelay) GetValidatorForSlot(nextSlot uint64) (ValidatorData, error) {
	r.requestedSlot = nextSlot
	return r.validator, nil
}
func (r *testRelay) handleRegisterValidator(w http.ResponseWriter, req *http.Request) {
}

var ErrValidatorNotFound = errors.New("validator not found")

type RemoteRelay struct {
	client http.Client
	config RelayConfig

	localRelay *LocalRelay

	cancellationsEnabled bool

	validatorsLock       sync.RWMutex
	validatorSyncOngoing bool
	lastRequestedSlot    uint64
	validatorSlotMap     map[uint64]ValidatorData
}

func NewRemoteRelay(config RelayConfig, localRelay *LocalRelay, cancellationsEnabled bool) *RemoteRelay {
	r := &RemoteRelay{
		config:               config,
		client:               http.Client{Timeout: time.Second},
		localRelay:           localRelay,
		cancellationsEnabled: cancellationsEnabled,
		validatorSyncOngoing: false,
		lastRequestedSlot:    0,
		validatorSlotMap:     make(map[uint64]ValidatorData),
	}

	err := r.updateValidatorsMap(0, 3)
	if err != nil {
		log.Error("could not connect to remote relay, continuing anyway", "err", err)
	}
	return r
}

func (r *RemoteRelay) Start() error {
	return nil
}

type GetValidatorRelayResponse []struct {
	Slot  uint64 `json:"slot,string"`
	Entry struct {
		Message struct {
			FeeRecipient string `json:"fee_recipient"`
			GasLimit     uint64 `json:"gas_limit,string"`
			Timestamp    uint64 `json:"timestamp,string"`
			Pubkey       string `json:"pubkey"`
		} `json:"message"`
		Signature string `json:"signature"`
	} `json:"entry"`
}

func (r *RemoteRelay) updateValidatorsMap(currentSlot uint64, retries int) error {
	r.validatorsLock.Lock()
	if r.validatorSyncOngoing {
		r.validatorsLock.Unlock()
		return errors.New("sync is ongoing")
	}
	r.validatorSyncOngoing = true
	r.validatorsLock.Unlock()

	log.Info("requesting ", "currentSlot", currentSlot)
	newMap, err := r.getSlotValidatorMapFromRelay()
	for err != nil && retries > 0 {
		log.Error("could not get validators map from relay, retrying", "err", err)
		time.Sleep(time.Second)
		newMap, err = r.getSlotValidatorMapFromRelay()
		retries -= 1
	}
	r.validatorsLock.Lock()
	r.validatorSyncOngoing = false
	if err != nil {
		r.validatorsLock.Unlock()
		log.Error("could not get validators map from relay", "err", err)
		return err
	}

	r.validatorSlotMap = newMap
	r.lastRequestedSlot = currentSlot
	r.validatorsLock.Unlock()

	log.Info("Updated validators", "new", newMap, "for slot", currentSlot)

	return nil
}

func (r *RemoteRelay) GetValidatorForSlot(nextSlot uint64) (ValidatorData, error) {
	// next slot is expected to be the actual chain's next slot, not something requested by the user!
	// if not sanitized it will force resync of validator data and possibly is a DoS vector

	r.validatorsLock.RLock()
	if r.lastRequestedSlot == 0 || nextSlot/32 > r.lastRequestedSlot/32 {
		// Every epoch request validators map
		go func() {
			err := r.updateValidatorsMap(nextSlot, 1)
			if err != nil {
				log.Error("could not update validators map", "err", err)
			}
		}()
	}

	vd, found := r.validatorSlotMap[nextSlot]
	r.validatorsLock.RUnlock()

	if r.localRelay != nil {
		localValidator, err := r.localRelay.GetValidatorForSlot(nextSlot)
		if err == nil {
			log.Info("Validator registration overwritten by local data", "slot", nextSlot, "validator", localValidator)
			return localValidator, nil
		}
	}

	if found {
		return vd, nil
	}

	return ValidatorData{}, ErrValidatorNotFound
}

func (r *RemoteRelay) SubmitBlock(msg *capellaapi.SubmitBlockRequest, vd ValidatorData) error {
	log.Info("submitting block to remote relay", "endpoint", r.config.Endpoint)

	endpoint := r.config.Endpoint + "/relay/v1/builder/blocks"
	if r.cancellationsEnabled {
		endpoint = endpoint + "?cancellations=true"
	}

	if r.config.SszEnabled {
		bodyBytes, err := msg.MarshalSSZ()
		if err != nil {
			return fmt.Errorf("error marshaling ssz: %w", err)
		}
		log.Debug("submitting block to remote relay", "endpoint", r.config.Endpoint)
		code, err := SendSSZRequest(context.TODO(), *http.DefaultClient, http.MethodPost, endpoint, bodyBytes, r.config.GzipEnabled)
		if err != nil {
			return fmt.Errorf("error sending http request to relay %s. err: %w", r.config.Endpoint, err)
		}
		if code > 299 {
			return fmt.Errorf("non-ok response code %d from relay %s", code, r.config.Endpoint)
		}
	} else {
		code, err := SendHTTPRequest(context.TODO(), *http.DefaultClient, http.MethodPost, endpoint, msg, nil)
		if err != nil {
			return fmt.Errorf("error sending http request to relay %s. err: %w", r.config.Endpoint, err)
		}
		if code > 299 {
			return fmt.Errorf("non-ok response code %d from relay %s", code, r.config.Endpoint)
		}
	}

	if r.localRelay != nil {
		r.localRelay.SubmitBlock(msg, vd)
	}

	return nil
}

func (r *RemoteRelay) getSlotValidatorMapFromRelay() (map[uint64]ValidatorData, error) {
	var dst GetValidatorRelayResponse
	code, err := SendHTTPRequest(context.Background(), *http.DefaultClient, http.MethodGet, r.config.Endpoint+"/relay/v1/builder/validators", nil, &dst)
	if err != nil {
		return nil, err
	}

	if code > 299 {
		return nil, fmt.Errorf("non-ok response code %d from relay", code)
	}

	res := make(map[uint64]ValidatorData)
	for _, data := range dst {
		feeRecipientBytes, err := hexutil.Decode(data.Entry.Message.FeeRecipient)
		if err != nil {
			log.Error("Ill-formatted fee_recipient from relay", "data", data)
			continue
		}
		feeRecipient := bellatrix.ExecutionAddress(feeRecipientBytes)

		pubkeyHex := PubkeyHex(strings.ToLower(data.Entry.Message.Pubkey))

		res[data.Slot] = ValidatorData{
			Pubkey:       pubkeyHex,
			FeeRecipient: feeRecipient,
			GasLimit:     data.Entry.Message.GasLimit,
		}
	}

	return res, nil
}
