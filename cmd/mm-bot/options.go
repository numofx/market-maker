package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/control"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/execution"
	"github.com/numofx/market-maker/internal/metrics"
	"github.com/numofx/market-maker/internal/state"
)

// runOptions is MM_OPTION_SERIES: one process quoting every listed strike and side of an option series from one
// subaccount. Each strike is its own bot (its own client, book, orders and persisted state), and they share what must
// be shared on one account: the signer, the order nonces (execution.NonceAllocator), the net delta they skew against
// (state.DeltaBook), and the operator's kill and pause (one controller, one control API, one kill-switch file).
//
// Metrics are per strike at /metrics/<market>; /metrics is the first strike's, for a scraper that reads one path.
func runOptions(ctx context.Context, cfg config.Config, series string, signer exchange.Signer, logger *slog.Logger) int {
	discovery, err := exchange.NewHTTPClient(ctx, clientConfig(cfg, "", signer))
	if err != nil {
		logger.Error("init exchange client", "error", err)
		return 1
	}
	specs := discovery.OptionMarkets(series)
	discovery.Close()
	if len(specs) == 0 {
		logger.Error("no option strikes listed for the series", "series", series)
		return 1
	}

	controlStore := state.NewStore(cfg.StateFile)
	controller, err := control.NewController(controlStore, cfg.HalfSpreadBPS)
	if err != nil {
		logger.Error("init controller", "error", err)
		return 1
	}
	nonces := execution.NewNonceAllocator()
	deltas := state.NewDeltaBook()
	mux := http.NewServeMux()
	var (
		bots     []*execution.Bot
		registry []*metrics.Registry
	)
	for _, listed := range specs {
		strikeCfg := cfg
		strikeCfg.MarketSymbol = listed.Symbol
		strikeCfg.StateFile = cfg.StateFile + "." + listed.Symbol
		// Ten strikes are ten pools on one small RDS (db.t4g.micro, ~80 connections): two each bounds the process at 20.
		strikeCfg.DatabaseURL = withPoolMaxConns(cfg.DatabaseURL, optionPoolMaxConns)
		client, err := exchange.NewHTTPClient(ctx, clientConfig(strikeCfg, listed.Symbol, signer))
		if err != nil {
			logger.Error("init exchange client", "error", err, "market", listed.Symbol)
			return 1
		}
		defer client.Close()
		spec, err := client.GetMarket(ctx, listed.Symbol)
		if err != nil {
			logger.Error("resolve market", "error", err, "market", listed.Symbol)
			return 1
		}
		if err := client.CheckOptionWiring(ctx, spec); err != nil {
			logger.Error("option wiring", "error", err, "market", spec.Symbol)
			return 1
		}
		// A strike whose chain state cannot be read at boot would never quote: say so now rather than every cycle.
		if st, err := client.OptionState(ctx, spec); err != nil {
			logger.Error("read option state", "error", err, "market", spec.Symbol)
			return 1
		} else {
			logger.Info("option strike", "market", spec.Symbol, "strike", spec.Option.Strike, "call", spec.Option.Call,
				"forward", st.Forward, "vol", st.Vol, "vol_confidence", st.VolConfidence, "vol_unavailable", st.VolUnavailable,
				"im_surplus", st.IMSurplus, "write_im_per_cngn", st.WriteIMPerBase, "unit", spec.Option.UnitBase)
		}
		reg := metrics.New()
		bot := execution.NewBot(strikeCfg, client, spec, reg, logger.With("market", spec.Symbol), state.NewStore(strikeCfg.StateFile))
		bot.AttachController(controller)
		bot.SetNonceAllocator(nonces)
		bot.SetDeltaBook(deltas)
		mux.Handle("/metrics/"+spec.Symbol, reg.Handler())
		bots = append(bots, bot)
		registry = append(registry, reg)
	}
	mux.Handle("/healthz", registry[0].HealthHandler())
	mux.Handle("/readyz", anyReady(registry))
	mux.Handle("/metrics", registry[0].Handler())

	controlServer := control.NewServer(cfg.ControlAddr, cfg.ControlToken, controller, optionBackends(bots), logger)
	if _, err := controlServer.Start(); err != nil {
		logger.Error("control API failed to start", "error", err)
		return 1
	}
	metricsServer := &http.Server{Addr: cfg.MetricsAddr, Handler: mux}
	go func() {
		if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server failed", "error", err)
		}
	}()

	for _, bot := range bots {
		if err := bot.Initialize(ctx); err != nil {
			logger.Error("startup reconciliation failed", "error", err)
			return 1
		}
	}
	logger.Info("options market maker started", "series", series, "strikes", len(bots), "dry_run", cfg.DryRun, "subaccount_id", cfg.SubaccountID)

	cycle := func(trigger string) {
		var wg sync.WaitGroup
		for _, bot := range bots {
			wg.Add(1)
			go func(b *execution.Bot) {
				defer wg.Done()
				if err := b.RunCycle(ctx); err != nil {
					logger.Error("run cycle failed", "error", err, "trigger", trigger)
				}
			}(bot)
		}
		wg.Wait()
	}
	cycle("startup")
	poll := time.NewTicker(cfg.PollInterval)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			for _, bot := range bots {
				logger.Info("shutdown summary", "summary", bot.ShutdownSummaryLine())
			}
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = controlServer.Shutdown(shutdownCtx)
			_ = metricsServer.Shutdown(shutdownCtx)
			return 0
		case <-poll.C:
			cycle("poll")
		case <-controller.Wake():
			cycle("control")
		}
	}
}

// anyReady is ready while any strike is: one strike pulled (its vol, its cutoff) is not the process being unwell.
func anyReady(regs []*metrics.Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var last *httptest.ResponseRecorder
		for _, reg := range regs {
			rec := httptest.NewRecorder()
			reg.ReadyHandler().ServeHTTP(rec, r)
			last = rec
			if rec.Code == http.StatusOK {
				break
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(last.Code)
		_, _ = w.Write(last.Body.Bytes())
	})
}

// optionBackends is the control API's view of every strike: cancels fan out to all of them; the view is the first
// strike's, with the market named as the series' strike count.
type optionBackends []*execution.Bot

func (o optionBackends) View() control.BotView {
	view := o[0].View()
	view.Market = fmt.Sprintf("%s (+%d strikes)", view.Market, len(o)-1)
	return view
}

func (o optionBackends) CancelManaged(ctx context.Context, scope control.CancelScope, side exchange.Side) ([]control.CancelResult, error) {
	var (
		out  []control.CancelResult
		errs []error
	)
	for _, bot := range o {
		res, err := bot.CancelManaged(ctx, scope, side)
		out = append(out, res...)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return out, errors.Join(errs...)
}

// optionPoolMaxConns is each strike's Postgres pool size in options mode.
const optionPoolMaxConns = 2

// withPoolMaxConns sets pgx's pool_max_conns on a Postgres URL or keyword/value DSN, replacing any already there.
func withPoolMaxConns(dsn string, n int) string {
	if u, err := url.Parse(dsn); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		q := u.Query()
		q.Set("pool_max_conns", strconv.Itoa(n))
		u.RawQuery = q.Encode()
		return u.String()
	}
	fields := strings.Fields(dsn)
	out := fields[:0]
	for _, f := range fields {
		if !strings.HasPrefix(f, "pool_max_conns=") {
			out = append(out, f)
		}
	}
	return strings.Join(append(out, "pool_max_conns="+strconv.Itoa(n)), " ")
}
