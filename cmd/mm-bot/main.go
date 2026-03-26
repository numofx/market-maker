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
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/execution"
	"github.com/numofx/market-maker/internal/metrics"
	"github.com/numofx/market-maker/internal/state"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client, err := exchange.NewHTTPClient(ctx, exchange.ClientConfig{
		APIBaseURL:         cfg.APIBaseURL,
		RPCURL:             cfg.RPCURL,
		DatabaseURL:        cfg.DatabaseURL,
		ChainID:            cfg.ChainID,
		MatchingRepoPath:   cfg.MatchingRepoPath,
		RiskCoreRepoPath:   cfg.RiskCoreRepoPath,
		MatchingAddress:    cfg.MatchingAddress,
		TradeModuleAddress: cfg.TradeModuleAddress,
		SubAccountsAddress: cfg.SubAccountsAddress,
		OwnerAddress:       cfg.OwnerAddress,
		SignerAddress:      cfg.SignerAddress,
		OwnerPrivateKey:    cfg.OwnerPrivateKey,
		SignerPrivateKey:   cfg.SignerPrivateKey,
		SubaccountID:       cfg.SubaccountID,
		RecipientID:        cfg.RecipientID,
		WorstFee:           cfg.WorstFee,
		OrderExpirySeconds: cfg.OrderExpirySeconds,
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
	bot := execution.NewBot(cfg, client, spec, metricRegistry, logger, state.NewStore(cfg.StateFile))

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

	logger.Info("market maker started", "market", spec.Symbol, "dry_run", cfg.DryRun, "subaccount_id", cfg.SubaccountID)
	if err := bot.RunCycle(ctx); err != nil {
		logger.Error("initial run cycle failed", "error", err)
	}

	for {
		select {
		case <-ctx.Done():
			logger.Info("shutdown requested")
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = metricsServer.Shutdown(shutdownCtx)
			return
		case <-pollTicker.C:
			if err := bot.RunCycle(ctx); err != nil {
				logger.Error("run cycle failed", "error", err)
			}
		}
	}
}

func metricsMux(reg *metrics.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/healthz", metrics.HealthHandler())
	mux.Handle("/metrics", reg.Handler())
	return mux
}
