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
)

type Config struct {
	APIBaseURL                string
	RPCURL                    string
	DatabaseURL               string
	ChainID                   int64
	MatchingRepoPath          string
	RiskCoreRepoPath          string
	MatchingAddress           string
	TradeModuleAddress        string
	SubAccountsAddress        string
	OwnerPrivateKey           string
	SignerPrivateKey          string
	OwnerAddress              string
	SignerAddress             string
	SubaccountID              string
	RecipientID               string
	WorstFee                  string
	OrderExpirySeconds        int64
	StateFile                 string
	MarketSymbol              string
	PollInterval              time.Duration
	QuoteRefreshInterval      time.Duration
	OrderSize                 float64
	HalfSpreadBPS             float64
	InventorySkewBPS          float64
	MaxLongInventory          float64
	MaxShortInventory         float64
	MinBaseBalance            float64
	MinQuoteBalance           float64
	CancelStaleOrderThreshold float64
	AdoptSizeTolerance        float64
	DryRun                    bool
	LogLevel                  slog.Level
	MetricsAddr               string
}

func Load() (Config, error) {
	cfg := Config{
		APIBaseURL:                strings.TrimRight(strings.TrimSpace(os.Getenv("MM_API_BASE_URL")), "/"),
		RPCURL:                    strings.TrimSpace(os.Getenv("MM_RPC_URL")),
		DatabaseURL:               envStringFallback([]string{"MM_DATABASE_URL", "DATABASE_URL"}, ""),
		ChainID:                   int64(envInt("MM_CHAIN_ID", 8453)),
		MatchingRepoPath:          envString("MM_MATCHING_REPO_PATH", "../execution-contracts"),
		RiskCoreRepoPath:          envString("MM_RISK_CORE_REPO_PATH", "../risk-core"),
		MatchingAddress:           strings.TrimSpace(os.Getenv("MM_MATCHING_ADDRESS")),
		TradeModuleAddress:        strings.TrimSpace(os.Getenv("MM_TRADE_MODULE_ADDRESS")),
		SubAccountsAddress:        strings.TrimSpace(os.Getenv("MM_SUBACCOUNTS_ADDRESS")),
		OwnerPrivateKey:           strings.TrimSpace(os.Getenv("MM_OWNER_PRIVATE_KEY")),
		SignerPrivateKey:          envStringFallback([]string{"MM_SIGNER_PRIVATE_KEY", "MM_OWNER_PRIVATE_KEY"}, ""),
		OwnerAddress:              strings.TrimSpace(os.Getenv("MM_OWNER_ADDRESS")),
		SignerAddress:             strings.TrimSpace(os.Getenv("MM_SIGNER_ADDRESS")),
		SubaccountID:              strings.TrimSpace(os.Getenv("MM_SUBACCOUNT_ID")),
		RecipientID:               strings.TrimSpace(os.Getenv("MM_RECIPIENT_ID")),
		WorstFee:                  envString("MM_WORST_FEE", defaultWorstFee),
		OrderExpirySeconds:        int64(envInt("MM_ORDER_EXPIRY_SECONDS", defaultExpirySeconds)),
		StateFile:                 envString("MM_STATE_FILE", filepath.Join(".", ".mm-bot-state.json")),
		MarketSymbol:              envString("MM_MARKET_SYMBOL", defaultMarket),
		PollInterval:              time.Duration(envInt("MM_POLL_INTERVAL_MS", defaultPollIntervalMS)) * time.Millisecond,
		QuoteRefreshInterval:      time.Duration(envInt("MM_QUOTE_REFRESH_INTERVAL_MS", defaultQuoteRefreshMS)) * time.Millisecond,
		OrderSize:                 envFloat("MM_ORDER_SIZE", defaultOrderSize),
		HalfSpreadBPS:             envFloat("MM_HALF_SPREAD_BPS", defaultHalfSpreadBPS),
		InventorySkewBPS:          envFloat("MM_INVENTORY_SKEW_BPS", defaultInventorySkewBPS),
		MaxLongInventory:          envFloat("MM_MAX_LONG_INVENTORY", defaultMaxLongInventory),
		MaxShortInventory:         envFloat("MM_MAX_SHORT_INVENTORY", defaultMaxShortInventory),
		MinBaseBalance:            envFloat("MM_MIN_BASE_BALANCE", defaultMinBaseBalance),
		MinQuoteBalance:           envFloat("MM_MIN_QUOTE_BALANCE", defaultMinQuoteBalance),
		CancelStaleOrderThreshold: envFloat("MM_CANCEL_STALE_ORDER_THRESHOLD_BPS", defaultCancelStaleThreshold),
		AdoptSizeTolerance:        envFloat("MM_ADOPT_SIZE_TOLERANCE", defaultAdoptSizeTolerance),
		DryRun:                    envBool("MM_DRY_RUN", false),
		MetricsAddr:               envString("MM_METRICS_ADDR", defaultMetricsAddr),
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
