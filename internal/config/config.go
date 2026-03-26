package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
)

const (
	defaultMarket               = "USDCcNGN-SPOT"
	defaultPollIntervalMS       = 2000
	defaultQuoteRefreshMS       = 5000
	defaultOrderSize            = 100
	defaultHalfSpreadBPS        = 25
	defaultInventorySkewBPS     = 15
	defaultMaxLongInventory     = 1000
	defaultMaxShortInventory    = -1000
	defaultMinBaseBalance       = 0
	defaultMinQuoteBalance      = 0
	defaultCancelStaleThreshold = 5
	defaultMetricsAddr          = ":8080"
	defaultLogLevel             = "INFO"
	defaultWorstFee             = "0"
	defaultExpirySeconds        = 3600
	defaultAdoptSizeTolerance   = 0.000001
	defaultOperatorMode         = "normal"
	defaultAnchorSourceType     = "none"
	defaultMaxCancelsPerMinute  = 30
	defaultSoakLogInterval      = 0
)

type OperatorMode string

const (
	ModeNormal       OperatorMode = "normal"
	ModePause        OperatorMode = "pause"
	ModeBidOnly      OperatorMode = "bid-only"
	ModeAskOnly      OperatorMode = "ask-only"
	ModeDryRunHealth OperatorMode = "dry-run-health"
)

type Config struct {
	APIBaseURL                   string
	RPCURL                       string
	DatabaseURL                  string
	ChainID                      int64
	MatchingRepoPath             string
	RiskCoreRepoPath             string
	MatchingAddress              string
	TradeModuleAddress           string
	SubAccountsAddress           string
	OwnerPrivateKey              string
	SignerPrivateKey             string
	OwnerAddress                 string
	SignerAddress                string
	SubaccountID                 string
	RecipientID                  string
	WorstFee                     string
	OrderExpirySeconds           int64
	StateFile                    string
	MarketSymbol                 string
	PollInterval                 time.Duration
	QuoteRefreshInterval         time.Duration
	OrderSize                    float64
	HalfSpreadBPS                float64
	InventorySkewBPS             float64
	MaxLongInventory             float64
	MaxShortInventory            float64
	MinBaseBalance               float64
	MinQuoteBalance              float64
	MaxNotionalPerSide           float64
	MaxNetInventory              float64
	MaxQuoteAge                  time.Duration
	MaxAnchorDeviationBPS        float64
	StaleMarketDataTimeout       time.Duration
	StaleBalanceTimeout          time.Duration
	StaleAnchorTimeout           time.Duration
	MinQuoteLifetime             time.Duration
	MinReplaceMoveBPS            float64
	MaxCancelsPerMinute          int
	CancelStaleOrderThreshold    float64
	AdoptSizeTolerance           float64
	OperatorMode                 OperatorMode
	AnchorSourceType             string
	AnchorURL                    string
	AnchorFixedPrice             float64
	KillSwitchFile               string
	DryRun                       bool
	LogLevel                     slog.Level
	MetricsAddr                  string
	ReadinessMissingQuoteTimeout time.Duration
	SoakLogInterval              time.Duration
	USDCCNGNSpotExternalAnchor   USDCCNGNSpotExternalAnchorConfig
}

type USDCCNGNSpotExternalAnchorConfig struct {
	Enabled          bool
	Provider         string
	BaseURL          string
	APIKey           string
	RPCURL           string
	ChainID          int64
	SellToken        string
	BuyToken         string
	Amount           string
	Timeout          time.Duration
	MaxAge           time.Duration
	MaxDeviationBPS  float64
	BootstrapOnly    bool
	SpreadMultiplier float64
	SizeMultiplier   float64
}

func Load() (Config, error) {
	cfg := Config{
		APIBaseURL:                   strings.TrimRight(strings.TrimSpace(os.Getenv("MM_API_BASE_URL")), "/"),
		RPCURL:                       strings.TrimSpace(os.Getenv("MM_RPC_URL")),
		DatabaseURL:                  envStringFallback([]string{"MM_DATABASE_URL", "DATABASE_URL"}, ""),
		ChainID:                      int64(envInt("MM_CHAIN_ID", 8453)),
		MatchingRepoPath:             envString("MM_MATCHING_REPO_PATH", "../execution-contracts"),
		RiskCoreRepoPath:             envString("MM_RISK_CORE_REPO_PATH", "../risk-core"),
		MatchingAddress:              strings.TrimSpace(os.Getenv("MM_MATCHING_ADDRESS")),
		TradeModuleAddress:           strings.TrimSpace(os.Getenv("MM_TRADE_MODULE_ADDRESS")),
		SubAccountsAddress:           strings.TrimSpace(os.Getenv("MM_SUBACCOUNTS_ADDRESS")),
		OwnerPrivateKey:              strings.TrimSpace(os.Getenv("MM_OWNER_PRIVATE_KEY")),
		SignerPrivateKey:             envStringFallback([]string{"MM_SIGNER_PRIVATE_KEY", "MM_OWNER_PRIVATE_KEY"}, ""),
		OwnerAddress:                 strings.TrimSpace(os.Getenv("MM_OWNER_ADDRESS")),
		SignerAddress:                strings.TrimSpace(os.Getenv("MM_SIGNER_ADDRESS")),
		SubaccountID:                 strings.TrimSpace(os.Getenv("MM_SUBACCOUNT_ID")),
		RecipientID:                  strings.TrimSpace(os.Getenv("MM_RECIPIENT_ID")),
		WorstFee:                     envString("MM_WORST_FEE", defaultWorstFee),
		OrderExpirySeconds:           int64(envInt("MM_ORDER_EXPIRY_SECONDS", defaultExpirySeconds)),
		StateFile:                    envString("MM_STATE_FILE", filepath.Join(".", ".mm-bot-state.json")),
		MarketSymbol:                 envString("MM_MARKET_SYMBOL", defaultMarket),
		PollInterval:                 time.Duration(envInt("MM_POLL_INTERVAL_MS", defaultPollIntervalMS)) * time.Millisecond,
		QuoteRefreshInterval:         time.Duration(envInt("MM_QUOTE_REFRESH_INTERVAL_MS", defaultQuoteRefreshMS)) * time.Millisecond,
		OrderSize:                    envFloat("MM_ORDER_SIZE", defaultOrderSize),
		HalfSpreadBPS:                envFloat("MM_HALF_SPREAD_BPS", defaultHalfSpreadBPS),
		InventorySkewBPS:             envFloat("MM_INVENTORY_SKEW_BPS", defaultInventorySkewBPS),
		MaxLongInventory:             envFloat("MM_MAX_LONG_INVENTORY", defaultMaxLongInventory),
		MaxShortInventory:            envFloat("MM_MAX_SHORT_INVENTORY", defaultMaxShortInventory),
		MaxNotionalPerSide:           envFloat("MM_MAX_NOTIONAL_PER_SIDE", 0),
		MaxNetInventory:              envFloat("MM_MAX_NET_INVENTORY", 0),
		MaxQuoteAge:                  time.Duration(envInt("MM_MAX_QUOTE_AGE_SECONDS", 0)) * time.Second,
		MaxAnchorDeviationBPS:        envFloat("MM_MAX_ANCHOR_DEVIATION_BPS", 0),
		StaleMarketDataTimeout:       time.Duration(envInt("MM_STALE_MARKET_DATA_TIMEOUT_SECONDS", 0)) * time.Second,
		StaleBalanceTimeout:          time.Duration(envInt("MM_STALE_BALANCE_TIMEOUT_SECONDS", 0)) * time.Second,
		StaleAnchorTimeout:           time.Duration(envInt("MM_STALE_ANCHOR_TIMEOUT_SECONDS", 0)) * time.Second,
		MinQuoteLifetime:             time.Duration(envInt("MM_MIN_QUOTE_LIFETIME_SECONDS", 0)) * time.Second,
		MinReplaceMoveBPS:            envFloat("MM_MIN_PRICE_MOVE_BEFORE_REPLACE_BPS", 0),
		MaxCancelsPerMinute:          envInt("MM_MAX_CANCELS_PER_MINUTE", defaultMaxCancelsPerMinute),
		MinBaseBalance:               envFloat("MM_MIN_BASE_BALANCE", defaultMinBaseBalance),
		MinQuoteBalance:              envFloat("MM_MIN_QUOTE_BALANCE", defaultMinQuoteBalance),
		CancelStaleOrderThreshold:    envFloat("MM_CANCEL_STALE_ORDER_THRESHOLD_BPS", defaultCancelStaleThreshold),
		AdoptSizeTolerance:           envFloat("MM_ADOPT_SIZE_TOLERANCE", defaultAdoptSizeTolerance),
		AnchorSourceType:             envString("MM_ANCHOR_SOURCE_TYPE", defaultAnchorSourceType),
		AnchorURL:                    strings.TrimSpace(os.Getenv("MM_ANCHOR_URL")),
		AnchorFixedPrice:             envFloat("MM_ANCHOR_FIXED_PRICE", 0),
		KillSwitchFile:               strings.TrimSpace(os.Getenv("MM_KILL_SWITCH_FILE")),
		DryRun:                       envBool("MM_DRY_RUN", false),
		MetricsAddr:                  envString("MM_METRICS_ADDR", defaultMetricsAddr),
		ReadinessMissingQuoteTimeout: time.Duration(envInt("MM_READINESS_MISSING_QUOTE_TIMEOUT_SECONDS", 0)) * time.Second,
		SoakLogInterval:              time.Duration(envInt("MM_SOAK_LOG_INTERVAL_SECONDS", defaultSoakLogInterval)) * time.Second,
		USDCCNGNSpotExternalAnchor: USDCCNGNSpotExternalAnchorConfig{
			Enabled:          envBool("MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_ENABLED", false),
			Provider:         envString("MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_PROVIDER", "0x"),
			BaseURL:          strings.TrimSpace(os.Getenv("MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_BASE_URL")),
			APIKey:           strings.TrimSpace(os.Getenv("MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_API_KEY")),
			RPCURL:           strings.TrimSpace(os.Getenv("MM_RPC_URL")),
			ChainID:          int64(envInt("MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_CHAIN_ID", 8453)),
			SellToken:        strings.TrimSpace(os.Getenv("MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_SELL_TOKEN")),
			BuyToken:         strings.TrimSpace(os.Getenv("MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_BUY_TOKEN")),
			Amount:           strings.TrimSpace(os.Getenv("MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_AMOUNT")),
			Timeout:          time.Duration(envInt("MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_TIMEOUT_MS", 1500)) * time.Millisecond,
			MaxAge:           time.Duration(envInt("MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_MAX_AGE_SECONDS", 30)) * time.Second,
			MaxDeviationBPS:  envFloat("MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_MAX_DEVIATION_BPS", 500),
			BootstrapOnly:    envBool("MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_BOOTSTRAP_ONLY", true),
			SpreadMultiplier: envFloat("MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_SPREAD_MULTIPLIER", 2.0),
			SizeMultiplier:   envFloat("MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_SIZE_MULTIPLIER", 0.5),
		},
	}

	if cfg.APIBaseURL == "" {
		return Config{}, fmt.Errorf("MM_API_BASE_URL is required")
	}
	if cfg.RPCURL == "" {
		return Config{}, fmt.Errorf("MM_RPC_URL is required")
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("MM_DATABASE_URL or DATABASE_URL is required")
	}
	if cfg.SubaccountID == "" {
		return Config{}, fmt.Errorf("MM_SUBACCOUNT_ID is required")
	}
	if cfg.OwnerPrivateKey == "" {
		return Config{}, fmt.Errorf("MM_OWNER_PRIVATE_KEY is required")
	}
	if _, err := crypto.HexToECDSA(strings.TrimPrefix(cfg.OwnerPrivateKey, "0x")); err != nil {
		return Config{}, fmt.Errorf("invalid MM_OWNER_PRIVATE_KEY: %w", err)
	}
	if _, err := crypto.HexToECDSA(strings.TrimPrefix(cfg.SignerPrivateKey, "0x")); err != nil {
		return Config{}, fmt.Errorf("invalid MM_SIGNER_PRIVATE_KEY: %w", err)
	}
	if cfg.OrderSize <= 0 {
		return Config{}, fmt.Errorf("MM_ORDER_SIZE must be > 0")
	}
	if cfg.MaxShortInventory > cfg.MaxLongInventory {
		return Config{}, fmt.Errorf("MM_MAX_SHORT_INVENTORY must be <= MM_MAX_LONG_INVENTORY")
	}
	if cfg.CancelStaleOrderThreshold < 0 {
		return Config{}, fmt.Errorf("MM_CANCEL_STALE_ORDER_THRESHOLD_BPS must be >= 0")
	}
	if cfg.MinReplaceMoveBPS < 0 {
		return Config{}, fmt.Errorf("MM_MIN_PRICE_MOVE_BEFORE_REPLACE_BPS must be >= 0")
	}
	if cfg.MaxCancelsPerMinute < 0 {
		return Config{}, fmt.Errorf("MM_MAX_CANCELS_PER_MINUTE must be >= 0")
	}
	mode, err := parseOperatorMode(envString("MM_OPERATOR_MODE", defaultOperatorMode))
	if err != nil {
		return Config{}, err
	}
	cfg.OperatorMode = mode
	if cfg.AnchorSourceType != "none" && cfg.AnchorSourceType != "fixed" && cfg.AnchorSourceType != "http" {
		return Config{}, fmt.Errorf("MM_ANCHOR_SOURCE_TYPE must be one of none, fixed, http")
	}
	if cfg.AnchorSourceType == "fixed" && cfg.AnchorFixedPrice <= 0 {
		return Config{}, fmt.Errorf("MM_ANCHOR_FIXED_PRICE must be > 0 when MM_ANCHOR_SOURCE_TYPE=fixed")
	}
	if cfg.AnchorSourceType == "http" && cfg.AnchorURL == "" {
		return Config{}, fmt.Errorf("MM_ANCHOR_URL is required when MM_ANCHOR_SOURCE_TYPE=http")
	}
	if cfg.USDCCNGNSpotExternalAnchor.Enabled {
		if cfg.MarketSymbol != "USDCcNGN-SPOT" {
			return Config{}, fmt.Errorf("MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_ENABLED is only supported for MM_MARKET_SYMBOL=USDCcNGN-SPOT")
		}
		if cfg.USDCCNGNSpotExternalAnchor.Provider != "0x" && cfg.USDCCNGNSpotExternalAnchor.Provider != "cngn-price-oracle" {
			return Config{}, fmt.Errorf("MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_PROVIDER must be one of 0x, cngn-price-oracle")
		}
		if cfg.USDCCNGNSpotExternalAnchor.Provider == "0x" {
			if cfg.USDCCNGNSpotExternalAnchor.BaseURL == "" {
				return Config{}, fmt.Errorf("MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_BASE_URL is required when external anchor provider is 0x")
			}
			if cfg.USDCCNGNSpotExternalAnchor.SellToken == "" {
				return Config{}, fmt.Errorf("MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_SELL_TOKEN is required when external anchor provider is 0x")
			}
			if cfg.USDCCNGNSpotExternalAnchor.BuyToken == "" {
				return Config{}, fmt.Errorf("MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_BUY_TOKEN is required when external anchor provider is 0x")
			}
			if cfg.USDCCNGNSpotExternalAnchor.Amount == "" {
				return Config{}, fmt.Errorf("MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_AMOUNT is required when external anchor provider is 0x")
			}
		}
		if cfg.USDCCNGNSpotExternalAnchor.Provider == "cngn-price-oracle" && cfg.USDCCNGNSpotExternalAnchor.RPCURL == "" {
			return Config{}, fmt.Errorf("MM_RPC_URL is required when external anchor provider is cngn-price-oracle")
		}
		if cfg.USDCCNGNSpotExternalAnchor.Timeout <= 0 {
			return Config{}, fmt.Errorf("MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_TIMEOUT_MS must be > 0")
		}
		if cfg.USDCCNGNSpotExternalAnchor.MaxAge <= 0 {
			return Config{}, fmt.Errorf("MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_MAX_AGE_SECONDS must be > 0")
		}
		if cfg.USDCCNGNSpotExternalAnchor.MaxDeviationBPS < 0 {
			return Config{}, fmt.Errorf("MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_MAX_DEVIATION_BPS must be >= 0")
		}
		if cfg.USDCCNGNSpotExternalAnchor.SpreadMultiplier < 1 {
			return Config{}, fmt.Errorf("MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_SPREAD_MULTIPLIER must be >= 1")
		}
		if cfg.USDCCNGNSpotExternalAnchor.SizeMultiplier <= 0 || cfg.USDCCNGNSpotExternalAnchor.SizeMultiplier > 1 {
			return Config{}, fmt.Errorf("MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_SIZE_MULTIPLIER must be > 0 and <= 1")
		}
	}

	level, err := parseLevel(envString("MM_LOG_LEVEL", defaultLogLevel))
	if err != nil {
		return Config{}, err
	}
	cfg.LogLevel = level
	return cfg, nil
}

func envString(key, fallback string) string {
	if raw := strings.TrimSpace(os.Getenv(key)); raw != "" {
		return raw
	}
	return fallback
}

func envStringFallback(keys []string, fallback string) string {
	for _, key := range keys {
		if raw := strings.TrimSpace(os.Getenv(key)); raw != "" {
			return raw
		}
	}
	return fallback
}

func envInt(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		panic(fmt.Sprintf("%s must be an integer: %v", key, err))
	}
	return value
}

func envFloat(key string, fallback float64) float64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		panic(fmt.Sprintf("%s must be a float: %v", key, err))
	}
	return value
}

func envBool(key string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		panic(fmt.Sprintf("%s must be a bool: %v", key, err))
	}
	return value
}

func parseLevel(raw string) (slog.Level, error) {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "DEBUG":
		return slog.LevelDebug, nil
	case "INFO":
		return slog.LevelInfo, nil
	case "WARN":
		return slog.LevelWarn, nil
	case "ERROR":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("MM_LOG_LEVEL must be one of DEBUG, INFO, WARN, ERROR")
	}
}

func parseOperatorMode(raw string) (OperatorMode, error) {
	switch OperatorMode(strings.ToLower(strings.TrimSpace(raw))) {
	case ModeNormal:
		return ModeNormal, nil
	case ModePause:
		return ModePause, nil
	case ModeBidOnly:
		return ModeBidOnly, nil
	case ModeAskOnly:
		return ModeAskOnly, nil
	case ModeDryRunHealth:
		return ModeDryRunHealth, nil
	default:
		return "", fmt.Errorf("MM_OPERATOR_MODE must be one of normal, pause, bid-only, ask-only, dry-run-health")
	}
}
