// book-maker quotes one market of risk-core's OnchainBook (Stage 2) from an account outside Matching: one spline,
// either oracle-pegged (repriced by updateSplines when the forward's basis to the index moves) or, on a v2 book,
// self-priced (MM_BOOK_SELF_PRICED: the forward itself, on a short ttl refreshed every cycle).
// See internal/onchainbook and docs/book-maker.md.
//
// It starts in dry run (MM_BOOK_DRY_RUN=true by default): it reads, decides and logs, and sends nothing.
package main

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/logging"
	"github.com/numofx/market-maker/internal/onchainbook"
)

type config struct {
	rpcURL            string
	book              common.Address
	marketID          uint64
	accountID         uint64
	referenceMarketID uint64
	referenceURL      string
	referenceMaxAge   time.Duration
	curveID           uint16
	curve             []onchainbook.Region
	params            onchainbook.Params
	poll              time.Duration
	dryRun            bool
	// heartbeatURL (MM_BOOK_HEARTBEAT_URL, optional, a secret) is pinged while the book is quoted: a dead-man check.
	heartbeatURL string
}

func main() {
	logger := slog.New(logging.NewRedactingHandler(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}),
		os.Getenv("MM_RPC_URL")))
	slog.SetDefault(logger)
	os.Exit(run(logger))
}

func run(log *slog.Logger) int {
	cfg, err := load()
	if err != nil {
		log.Error("config", "error", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	signer, err := newSigner(ctx)
	if err != nil {
		log.Error("signer", "error", err)
		return 1
	}
	subAccounts, err := onchainbook.BookSubAccounts(ctx, cfg.rpcURL, cfg.book)
	if err != nil {
		log.Error("book", "error", err)
		return 1
	}
	chain, err := onchainbook.NewChain(ctx, cfg.rpcURL, signer, cfg.book, subAccounts, cfg.marketID, cfg.accountID)
	if err != nil {
		log.Error("chain", "error", err)
		return 1
	}
	defer chain.Close()
	if cfg.referenceURL != "" {
		chain.HTTPReference = &onchainbook.HTTPReference{URL: cfg.referenceURL, MaxAge: cfg.referenceMaxAge}
		log.Info("reference from the rate service, not an engine feed", "url", cfg.referenceURL, "max_age", cfg.referenceMaxAge)
	}
	log.Info("book-maker starting", "book", cfg.book.Hex(), "market", cfg.marketID, "account", cfg.accountID,
		"owner", signer.Address().Hex(), "chain_id", chain.ChainID(), "dry_run", cfg.dryRun, "curve", cfg.curve,
		"half_spread_ticks", cfg.params.HalfSpreadTicks, "ttl", cfg.params.TTL, "book_v2", chain.V2,
		"self_priced", cfg.params.SelfPriced, "max_position_lots", cfg.params.MaxPositionLots,
		"skew_ticks_per_lot", cfg.params.SkewTicksPerLot, "max_skew_ticks", cfg.params.MaxSkewTicks)

	market, feeds, err := onchainbook.Preflight(ctx, chain, signer.Address(), cfg.params, cfg.curveID, cfg.curve,
		cfg.referenceMarketID, cfg.dryRun, log)
	if err != nil {
		log.Error("preflight refused", "error", err)
		return 1
	}
	maker := &onchainbook.Maker{Book: chain, Params: cfg.params, Feeds: feeds, Market: market, Owner: signer.Address(),
		DryRun: cfg.dryRun, Log: log}

	beat := &onchainbook.Heartbeat{URL: cfg.heartbeatURL, Every: time.Minute}
	log.Info("heartbeat", "configured", cfg.heartbeatURL != "")

	ticker := time.NewTicker(cfg.poll)
	defer ticker.Stop()
	for {
		d, err := maker.Cycle(ctx)
		if onchainbook.ShouldBeat(d, err, cfg.dryRun) {
			if berr := beat.Beat(ctx); berr != nil {
				log.Warn("heartbeat", "error", berr)
			}
		}
		if err != nil {
			if errors.Is(err, onchainbook.ErrSuperseded) {
				// Another process owns the spline: stop without cancelling it.
				log.Warn("superseded", "error", err)
				return 0
			}
			if ctx.Err() == nil {
				log.Error("cycle failed", "error", err)
			}
		}
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := maker.Shutdown(shutdownCtx); err != nil {
				log.Error("shutdown cancel failed: the spline lapses at its ttl", "error", err, "ttl", cfg.params.TTL)
				return 1
			}
			return 0
		case <-ticker.C:
		}
	}
}

func load() (config, error) {
	var cfg config
	var problems []string
	need := func(key string) string {
		v := strings.TrimSpace(os.Getenv(key))
		if v == "" {
			problems = append(problems, key+" is required")
		}
		return v
	}
	num := func(key string, fallback uint64) uint64 {
		v := strings.TrimSpace(os.Getenv(key))
		if v == "" {
			return fallback
		}
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s=%q is not a non-negative integer", key, v))
		}
		return n
	}
	required := func(key string) uint64 {
		v := need(key)
		if v == "" {
			return 0
		}
		return num(key, 0)
	}

	cfg.rpcURL = need("MM_RPC_URL")
	if b := need("MM_BOOK_ADDRESS"); b != "" {
		if !common.IsHexAddress(b) {
			problems = append(problems, "MM_BOOK_ADDRESS is not an address")
		}
		cfg.book = common.HexToAddress(b)
	}
	cfg.marketID = num("MM_BOOK_MARKET_ID", 1)
	cfg.accountID = required("MM_BOOK_ACCOUNT_ID")
	cfg.referenceMarketID = num("MM_BOOK_REFERENCE_MARKET_ID", 2)
	// cNGN: the engine's only cNGN price is the index itself, so the reference is the rate service (cngn-rate-picker).
	cfg.referenceURL = envOr("MM_BOOK_REFERENCE_URL", "")
	cfg.referenceMaxAge = time.Duration(num("MM_BOOK_REFERENCE_MAX_AGE_SECONDS", 300)) * time.Second
	if cfg.referenceURL != "" && !strings.HasPrefix(cfg.referenceURL, "http://") && !strings.HasPrefix(cfg.referenceURL, "https://") {
		problems = append(problems, "MM_BOOK_REFERENCE_URL must be an http(s) URL")
	}
	cfg.curveID = uint16(num("MM_BOOK_CURVE_ID", 1))
	if spec := need("MM_BOOK_CURVE"); spec != "" {
		curve, err := onchainbook.ParseCurve(spec)
		if err != nil {
			problems = append(problems, "MM_BOOK_CURVE: "+err.Error())
		}
		cfg.curve = curve
	}
	selfPriced := false
	switch strings.ToLower(envOr("MM_BOOK_SELF_PRICED", "false")) {
	case "true", "1":
		selfPriced = true
	case "false", "0":
	default:
		problems = append(problems, "MM_BOOK_SELF_PRICED must be true or false")
	}
	// Self-priced defaults: a 30s quote refreshed once 22s old (owner, 2026-11-17: 30s rather than C6's ~10s, for
	// gas; 22 + 5 + 3 = 30, CheckCadence), a reprice sending at once; pegged: an hour, refreshed at 30m.
	ttlDefault, refreshDefault := uint64(3600), uint64(1800)
	if selfPriced {
		ttlDefault, refreshDefault = 30, 22
	}
	divergence, err := strconv.ParseFloat(envOr("MM_BOOK_MAX_DIVERGENCE_BPS", "50"), 64)
	if err != nil {
		problems = append(problems, "MM_BOOK_MAX_DIVERGENCE_BPS is not a number")
	}
	cfg.params = onchainbook.Params{
		HalfSpreadTicks:       uint32(required("MM_BOOK_HALF_SPREAD_TICKS")),
		BidCurve:              cfg.curveID,
		AskCurve:              cfg.curveID,
		SkewTicksPerLot:       uint32(num("MM_BOOK_SKEW_TICKS_PER_LOT", 0)),
		MaxSkewTicks:          uint32(num("MM_BOOK_MAX_SKEW_TICKS", 0)),
		RefillLotsPerSecond:   uint32(num("MM_BOOK_REFILL_LOTS_PER_SECOND", 0)),
		TTL:                   time.Duration(num("MM_BOOK_TTL_SECONDS", ttlDefault)) * time.Second,
		RefreshAfter:          time.Duration(num("MM_BOOK_REFRESH_SECONDS", refreshDefault)) * time.Second,
		RepriceTicks:          int64(num("MM_BOOK_REPRICE_TICKS", 20)),
		CutoffBeforeLastTrade: time.Duration(num("MM_BOOK_CUTOFF_SECONDS", 3600)) * time.Second,
		MaxDivergenceBPS:      divergence,
		SelfPriced:            selfPriced,
		MaxPositionLots:       num("MM_BOOK_MAX_POSITION_LOTS", 0),
		ParkCurve:             uint16(num("MM_BOOK_PARK_CURVE_ID", uint64(cfg.curveID)+1)),
	}
	for _, r := range cfg.curve {
		cfg.params.CurveLots += uint64(r.Lots)
	}
	if err := cfg.params.Validate(); err != nil {
		problems = append(problems, err.Error())
	}
	cfg.poll = time.Duration(num("MM_BOOK_POLL_SECONDS", 5)) * time.Second
	if cfg.poll <= 0 {
		problems = append(problems, "MM_BOOK_POLL_SECONDS must be positive")
	} else if err := onchainbook.CheckCadence(cfg.params, cfg.poll); err != nil {
		problems = append(problems, err.Error())
	}
	cfg.heartbeatURL = strings.TrimSpace(os.Getenv("MM_BOOK_HEARTBEAT_URL"))
	switch strings.ToLower(envOr("MM_BOOK_DRY_RUN", "true")) {
	case "true", "1":
		cfg.dryRun = true
	case "false", "0":
		cfg.dryRun = false
	default:
		problems = append(problems, "MM_BOOK_DRY_RUN must be true or false")
	}
	if len(problems) > 0 {
		return cfg, errors.New(strings.Join(problems, "; "))
	}
	return cfg, nil
}

// newSigner is the bot's: MM_SIGNER_BACKEND=kms with MM_KMS_KEY_ID, or local with MM_OWNER_PRIVATE_KEY. The owner
// of MM_BOOK_ACCOUNT_ID signs and pays gas.
func newSigner(ctx context.Context) (exchange.Signer, error) {
	switch strings.ToLower(envOr("MM_SIGNER_BACKEND", "local")) {
	case "kms":
		keyID := strings.TrimSpace(os.Getenv("MM_KMS_KEY_ID"))
		if keyID == "" {
			return nil, errors.New("MM_SIGNER_BACKEND=kms needs MM_KMS_KEY_ID")
		}
		kctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		return exchange.NewAWSKMSSigner(kctx, keyID)
	case "local":
		raw := strings.TrimPrefix(strings.TrimSpace(os.Getenv("MM_OWNER_PRIVATE_KEY")), "0x")
		if raw == "" {
			return nil, errors.New("MM_OWNER_PRIVATE_KEY is required with the local signer")
		}
		var key *ecdsa.PrivateKey
		key, err := crypto.HexToECDSA(raw)
		if err != nil {
			return nil, errors.New("MM_OWNER_PRIVATE_KEY is not a private key")
		}
		return exchange.NewLocalSigner(key), nil
	default:
		return nil, errors.New("MM_SIGNER_BACKEND must be local or kms")
	}
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
