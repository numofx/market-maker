package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/control"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/execution"
	"github.com/numofx/market-maker/internal/metrics"
	"github.com/numofx/market-maker/internal/state"
)

func main() {
	if isHealthcheckArg(os.Args) {
		os.Exit(runHealthcheck())
	}

	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A nil interface, not a nil *KMSSigner, when the backend is local: the client tests Signer != nil
	// to decide whether the private keys are in play.
	var signer exchange.Signer
	if cfg.SignerBackend == config.SignerBackendKMS {
		if cfg.OwnerPrivateKey != "" || cfg.SignerPrivateKey != "" {
			logger.Warn("MM_SIGNER_BACKEND=kms ignores MM_OWNER_PRIVATE_KEY/MM_SIGNER_PRIVATE_KEY; remove them from the environment")
		}
		kmsCtx, cancelKMS := context.WithTimeout(ctx, 15*time.Second)
		kmsSigner, err := exchange.NewAWSKMSSigner(kmsCtx, cfg.KMSKeyID)
		cancelKMS()
		if err != nil {
			logger.Error("init kms signer", "error", err, "key_id", cfg.KMSKeyID)
			os.Exit(1)
		}
		logger.Info("signer backend", "backend", config.SignerBackendKMS, "key_id", cfg.KMSKeyID, "address", kmsSigner.Address().Hex())
		signer = kmsSigner
	}

	client, err := exchange.NewHTTPClient(ctx, exchange.ClientConfig{
		APIBaseURL:           cfg.APIBaseURL,
		RPCURL:               cfg.RPCURL,
		DatabaseURL:          cfg.DatabaseURL,
		MarketSymbol:         cfg.MarketSymbol,
		ChainID:              cfg.ChainID,
		MatchingRepoPath:     cfg.MatchingRepoPath,
		RiskCoreRepoPath:     cfg.RiskCoreRepoPath,
		MatchingAddress:      cfg.MatchingAddress,
		TradeModuleAddress:   cfg.TradeModuleAddress,
		SubAccountsAddress:   cfg.SubAccountsAddress,
		OwnerAddress:         cfg.OwnerAddress,
		SignerAddress:        cfg.SignerAddress,
		OwnerPrivateKey:      cfg.OwnerPrivateKey,
		SignerPrivateKey:     cfg.SignerPrivateKey,
		SubaccountID:         cfg.SubaccountID,
		RecipientID:          cfg.RecipientID,
		WorstFee:             cfg.WorstFee,
		MarketRefreshSeconds: cfg.MarketRefreshSeconds,
		OrderExpirySeconds:   cfg.OrderExpirySeconds,
		ServiceName:          cfg.ServiceName,
		ProtectedPrefixes:    cfg.ProtectedOrderIDPrefixes,
		Signer:               signer,
	})
	if err != nil {
		logger.Error("init exchange client", "error", err)
		os.Exit(1)
	}
	defer client.Close()

	spec, err := client.GetMarket(ctx, cfg.MarketSymbol)
	if err != nil {
		logger.Error("resolve market", "error", err, "market", cfg.MarketSymbol)
		os.Exit(1)
	}

	metricRegistry := metrics.New()
	store := state.NewStore(cfg.StateFile)
	// Built even when the control API is disabled: a kill or pause persisted by an earlier run must
	// still hold if the token has since been removed from the task definition.
	controller, err := control.NewController(store, cfg.HalfSpreadBPS)
	if err != nil {
		logger.Error("init controller", "error", err)
		os.Exit(1)
	}
	bot := execution.NewBot(cfg, client, spec, metricRegistry, logger, store)
	bot.AttachController(controller)

	// Started before Initialize, so a kill is available while startup reconciliation is still running.
	controlServer := control.NewServer(cfg.ControlAddr, cfg.ControlToken, controller, bot, logger)
	if _, err := controlServer.Start(); err != nil {
		logger.Error("control API failed to start", "error", err)
		os.Exit(1)
	}

	metricsServer := &http.Server{
		Addr:    cfg.MetricsAddr,
		Handler: metricsMux(metricRegistry),
	}
	go func() {
		logger.Info("metrics listening", "addr", cfg.MetricsAddr)
		if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server failed", "error", err)
		}
	}()

	if err := bot.Initialize(ctx); err != nil {
		logger.Error("startup reconciliation failed", "error", err)
		os.Exit(1)
	}

	pollTicker := time.NewTicker(cfg.PollInterval)
	defer pollTicker.Stop()
	var soakTicker *time.Ticker
	if cfg.SoakLogInterval > 0 {
		soakTicker = time.NewTicker(cfg.SoakLogInterval)
		defer soakTicker.Stop()
	}

	logger.Info("market maker started", "market", spec.Symbol, "dry_run", cfg.DryRun, "subaccount_id", cfg.SubaccountID)
	if err := bot.RunCycle(ctx); err != nil {
		logger.Error("initial run cycle failed", "error", err)
	}

	for {
		select {
		case <-ctx.Done():
			logger.Info("shutdown requested")
			logger.Info("shutdown summary", "summary", bot.ShutdownSummaryLine())
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = controlServer.Shutdown(shutdownCtx)
			_ = metricsServer.Shutdown(shutdownCtx)
			return
		case <-pollTicker.C:
			if err := bot.RunCycle(ctx); err != nil {
				logger.Error("run cycle failed", "error", err)
			}
		case <-controller.Wake():
			// An operator change runs a cycle now rather than at the next poll: a kill's follow-up
			// cancel, a pause, a pulled side or an adjust should not wait out MM_POLL_INTERVAL_MS.
			if err := bot.RunCycle(ctx); err != nil {
				logger.Error("run cycle failed", "error", err, "trigger", "control")
			}
		case <-soakTick(soakTicker):
			logger.Info("soak status", "status", bot.SoakStatusLine())
		}
	}
}

func metricsMux(reg *metrics.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/healthz", reg.HealthHandler())
	mux.Handle("/readyz", reg.ReadyHandler())
	mux.Handle("/metrics", reg.Handler())
	return mux
}

func soakTick(t *time.Ticker) <-chan time.Time {
	if t == nil {
		return nil
	}
	return t.C
}
