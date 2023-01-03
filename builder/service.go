package builder

import (
	"errors"
	"fmt"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/flashbots/go-boost-utils/ssz"
	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/common/hexutil"
	"github.com/ledgerwatch/erigon/mev"
	"github.com/ledgerwatch/erigon/node"
	"github.com/ledgerwatch/erigon/rpc"
	"github.com/ledgerwatch/log/v3"
	"golang.org/x/time/rate"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"github.com/flashbots/go-boost-utils/bls"
	"github.com/flashbots/go-utils/httplogger"
)

const (
	_PathStatus            = "/eth/v1/builder/status"
	_PathRegisterValidator = "/eth/v1/builder/validators"
	_PathGetHeader         = "/eth/v1/builder/header/{slot:[0-9]+}/{parent_hash:0x[a-fA-F0-9]+}/{pubkey:0x[a-fA-F0-9]+}"
	_PathGetPayload        = "/eth/v1/builder/blinded_blocks"
)

type Service struct {
	srv     *http.Server
	builder IBuilder
}

func (s *Service) Start() {
	if s.srv != nil {
		log.Info("Service started")
		go s.srv.ListenAndServe()
	}
	s.builder.Start()
}

func (s *Service) PayloadAttributes(payloadAttributes *BuilderPayloadAttributes) error {
	return s.builder.OnPayloadAttribute(payloadAttributes)
}

func getRouter(localRelay *LocalRelay) http.Handler {
	router := mux.NewRouter()

	// Add routes
	router.HandleFunc("/", localRelay.handleIndex).Methods(http.MethodGet)
	router.HandleFunc(_PathStatus, localRelay.handleStatus).Methods(http.MethodGet)
	router.HandleFunc(_PathRegisterValidator, localRelay.handleRegisterValidator).Methods(http.MethodPost)
	router.HandleFunc(_PathGetHeader, localRelay.handleGetHeader).Methods(http.MethodGet)
	router.HandleFunc(_PathGetPayload, localRelay.handleGetPayload).Methods(http.MethodPost)

	// Add logging and return router
	loggedRouter := httplogger.LoggingMiddleware(router)
	return loggedRouter
}

func getRelayConfig(endpoint string) (RelayConfig, error) {
	configs := strings.Split(endpoint, ";")
	if len(configs) == 0 {
		return RelayConfig{}, fmt.Errorf("empty relay endpoint %s", endpoint)
	}
	relayUrl := configs[0]
	// relay endpoint is configurated in the format URL;ssz=<value>;gzip=<value>
	// if any of them are missing, we default the config value to false
	var sszEnabled, gzipEnabled bool
	var err error

	for _, config := range configs {
		if strings.HasPrefix(config, "ssz=") {
			sszEnabled, err = strconv.ParseBool(config[4:])
			if err != nil {
				log.Info("invalid ssz config for relay", "endpoint", endpoint, "err", err)
			}
		} else if strings.HasPrefix(config, "gzip=") {
			gzipEnabled, err = strconv.ParseBool(config[5:])
			if err != nil {
				log.Info("invalid gzip config for relay", "endpoint", endpoint, "err", err)
			}
		}
	}
	return RelayConfig{
		Endpoint:    relayUrl,
		SszEnabled:  sszEnabled,
		GzipEnabled: gzipEnabled,
	}, nil
}

func NewService(listenAddr string, localRelay *LocalRelay, builder *Builder) *Service {
	var srv *http.Server
	if localRelay != nil {
		srv = &http.Server{
			Addr:    listenAddr,
			Handler: getRouter(localRelay),
			/*
			   ReadTimeout:
			   ReadHeaderTimeout:
			   WriteTimeout:
			   IdleTimeout:
			*/
		}
	}

	return &Service{
		srv:     srv,
		builder: builder,
	}
}

func Register(stack *node.Node, backend IEthereum, cfg *Config, logger log.Logger) error {
	envRelaySkBytes, err := hexutil.Decode(cfg.RelaySecretKey)
	if err != nil {
		return errors.New("relay secret key did not parse correctly")
	}

	relaySk, err := bls.SecretKeyFromBytes(envRelaySkBytes[:])
	if err != nil {
		return errors.New("incorrect relay secret key provided")
	}

	envBuilderSkBytes, err := hexutil.Decode(cfg.BuilderSecretKey)
	if err != nil {
		return errors.New("builder secret key did not parse correctly")
	}

	builderSk, err := bls.SecretKeyFromBytes(envBuilderSkBytes[:])
	if err != nil {
		log.Error("Secret key not correct", "providedSK", builderSk, "err", err)
		return errors.New("incorrect builder secret key provided")
	}

	genesisForkVersionBytes, err := hexutil.Decode(cfg.GenesisForkVersion)
	if err != nil {
		return fmt.Errorf("invalid genesisForkVersion: %w", err)
	}

	var genesisForkVersion [4]byte
	copy(genesisForkVersion[:], genesisForkVersionBytes[:4])
	builderSigningDomain := ssz.ComputeDomain(ssz.DomainTypeAppBuilder, genesisForkVersion, phase0.Root{})

	genesisValidatorsRoot := phase0.Root(common.HexToHash(cfg.GenesisValidatorsRoot))
	bellatrixForkVersionBytes, err := hexutil.Decode(cfg.BellatrixForkVersion)
	if err != nil {
		return fmt.Errorf("invalid bellatrixForkVersion: %w", err)
	}

	var bellatrixForkVersion [4]byte
	copy(bellatrixForkVersion[:], bellatrixForkVersionBytes[:4])
	proposerSigningDomain := ssz.ComputeDomain(ssz.DomainTypeBeaconProposer, bellatrixForkVersion, genesisValidatorsRoot)

	// Set up builder rate limiter based on environment variables or CLI flags.
	// Builder rate limit parameters are flags.BuilderRateLimitDuration and flags.BuilderRateLimitMaxBurst
	duration, err := time.ParseDuration(cfg.BuilderRateLimitDuration)
	if err != nil {
		return fmt.Errorf("error parsing builder rate limit duration - %w", err)
	}

	// BuilderRateLimitMaxBurst is set to builder.RateLimitBurstDefault by default if not specified
	limiter := rate.NewLimiter(rate.Every(duration), cfg.BuilderRateLimitMaxBurst)

	var builderRateLimitInterval time.Duration
	if cfg.BuilderRateLimitResubmitInterval != "" {
		d, err := time.ParseDuration(cfg.BuilderRateLimitResubmitInterval)
		if err != nil {
			return fmt.Errorf("error parsing builder rate limit resubmit interval - %v", err)
		}
		builderRateLimitInterval = d
	} else {
		builderRateLimitInterval = RateLimitIntervalDefault
	}

	var submissionOffset time.Duration
	if offset := cfg.BuilderSubmissionOffset; offset != 0 {
		if offset < 0 {
			return fmt.Errorf("builder submission offset must be positive")
		} else if uint64(offset.Seconds()) > cfg.SecondsInSlot {
			return fmt.Errorf("builder submission offset must be less than seconds in slot")
		}
		submissionOffset = offset
	} else {
		submissionOffset = SubmissionOffsetFromEndOfSlotSecondsDefault
	}

	var beaconClient IBeaconClient
	if len(cfg.BeaconEndpoints) == 0 {
		beaconClient = &NilBeaconClient{}
	} else if len(cfg.BeaconEndpoints) == 1 {
		beaconClient = NewBeaconClient(cfg.BeaconEndpoints[0], cfg.SlotsInEpoch, cfg.SecondsInSlot)
	} else {
		beaconClient = NewMultiBeaconClient(cfg.BeaconEndpoints, cfg.SlotsInEpoch, cfg.SecondsInSlot)
	}

	var localRelay *LocalRelay
	if cfg.EnableLocalRelay {
		localRelay = NewLocalRelay(relaySk, beaconClient, builderSigningDomain, proposerSigningDomain, ForkData{cfg.GenesisForkVersion, cfg.BellatrixForkVersion, cfg.GenesisValidatorsRoot}, cfg.EnableValidatorChecks)
	}

	var relay IRelay
	if cfg.RemoteRelayEndpoint != "" {
		relayConfig, err := getRelayConfig(cfg.RemoteRelayEndpoint)
		if err != nil {
			return fmt.Errorf("invalid remote relay endpoint: %w", err)
		}
		relay = NewRemoteRelay(relayConfig, localRelay, cfg.EnableCancellations)
	} else if localRelay != nil {
		relay = localRelay
	} else {
		return errors.New("neither local nor remote relay specified")
	}

	if len(cfg.SecondaryRemoteRelayEndpoints) > 0 && !(len(cfg.SecondaryRemoteRelayEndpoints) == 1 && cfg.SecondaryRemoteRelayEndpoints[0] == "") {
		secondaryRelays := make([]IRelay, len(cfg.SecondaryRemoteRelayEndpoints))
		for i, endpoint := range cfg.SecondaryRemoteRelayEndpoints {
			relayConfig, err := getRelayConfig(endpoint)
			if err != nil {
				return fmt.Errorf("invalid secondary remote relay endpoint: %w", err)
			}
			secondaryRelays[i] = NewRemoteRelay(relayConfig, nil, cfg.EnableCancellations)
		}
		relay = NewRemoteRelayAggregator(relay, secondaryRelays)
	}

	bundlePool := mev.NewBundlePool()
	ethereumService := NewEthereumService(backend, bundlePool, logger)

	builderArgs := BuilderArgs{
		feeRecipientPk:                cfg.BuilderFeeRecipientPrivateKey,
		sk:                            builderSk,
		relay:                         relay,
		builderSigningDomain:          builderSigningDomain,
		builderBlockResubmitInterval:  builderRateLimitInterval,
		discardRevertibleTxOnErr:      cfg.DiscardRevertibleTxOnErr,
		eth:                           ethereumService,
		dryRun:                        cfg.DryRun,
		ignoreLatePayloadAttributes:   cfg.IgnoreLatePayloadAttributes,
		beaconClient:                  beaconClient,
		submissionOffsetFromEndOfSlot: submissionOffset,
		blacklistFile:                 cfg.ValidationBlocklist,
		limiter:                       limiter,
	}

	builderBackend := NewBuilder(builderArgs)
	builderService := NewService(cfg.ListenAddr, localRelay, builderBackend)
	builderService.Start()

	stack.RegisterPublicAPIs([]rpc.API{
		{
			Namespace: "eth",
			Version:   "1.0",
			Service:   mev.NewBundlePoolApi(bundlePool),
			Public:    true,
		},
	})

	stack.RegisterAuthenticatedAPIs([]rpc.API{
		{
			Namespace: "builder",
			Version:   "1.0",
			Service:   builderService,
			Public:    true,
		},
	})
	return nil
}
