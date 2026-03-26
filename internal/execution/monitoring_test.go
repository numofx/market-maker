package execution

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/metrics"
	"github.com/numofx/market-maker/internal/state"
	"github.com/numofx/market-maker/internal/strategy"
)

func TestReadinessFailsOnStaleDependencies(t *testing.T) {
	reg := metrics.New()
	bot := NewBot(config.Config{
		OperatorMode:                 config.ModeNormal,
		StaleMarketDataTimeout:       time.Second,
		StaleBalanceTimeout:          time.Second,
		StaleAnchorTimeout:           time.Second,
		ReadinessMissingQuoteTimeout: time.Minute,
	}, &mockClient{}, exchange.MarketSpec{BaseAsset: "USDC", QuoteAsset: "cNGN"}, reg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	now := time.Now().UTC()
	bot.updateReadiness(state.Snapshot{
		LastMarketDataRefresh: now,
		LastBalanceRefresh:    now,
		LastAnchorRefresh:     now.Add(-2 * time.Second),
		AnchorSource:          "fixed",
	}, strategy.Result{})

	rr := httptest.NewRecorder()
	reg.ReadyHandler().ServeHTTP(rr, httptest.NewRequest("GET", "/readyz", nil))
	if rr.Code != 503 || !strings.Contains(rr.Body.String(), "anchor data stale") {
		t.Fatalf("readyz = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestReadinessFailsWhenRequiredQuotesMissing(t *testing.T) {
	reg := metrics.New()
	bot := NewBot(config.Config{
		OperatorMode:                 config.ModeNormal,
		ReadinessMissingQuoteTimeout: time.Second,
	}, &mockClient{}, exchange.MarketSpec{BaseAsset: "USDC", QuoteAsset: "cNGN"}, reg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	bot.updateReadiness(state.Snapshot{
		LastQuoteUpdate: time.Now().UTC().Add(-2 * time.Second),
		OpenOrders:      nil,
	}, strategy.Result{
		Bid: &strategy.Quote{Side: exchange.SideBuy, Price: 100, Size: 1},
		Ask: &strategy.Quote{Side: exchange.SideSell, Price: 101, Size: 1},
	})

	rr := httptest.NewRecorder()
	reg.ReadyHandler().ServeHTTP(rr, httptest.NewRequest("GET", "/readyz", nil))
	if rr.Code != 503 || !strings.Contains(rr.Body.String(), "required bid missing too long") {
		t.Fatalf("readyz = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestStatusSummaryFieldsUpdate(t *testing.T) {
	reg := metrics.New()
	bot := NewBot(config.Config{
		OperatorMode: config.ModeNormal,
	}, &mockClient{}, exchange.MarketSpec{BaseAsset: "USDC", QuoteAsset: "cNGN"}, reg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	bot.snapshot = state.Snapshot{
		ReferencePrice:        100,
		InventoryByAsset:      map[string]float64{"USDC": 12.5, "cNGN": 1000},
		LastMarketDataRefresh: time.Now().UTC().Add(-3 * time.Second),
		LastBalanceRefresh:    time.Now().UTC().Add(-4 * time.Second),
		LastAnchorRefresh:     time.Now().UTC().Add(-5 * time.Second),
		LocalQuoteAge:         6 * time.Second,
		ExchangeQuoteAge:      7 * time.Second,
		OpenOrders: []exchange.Order{
			{ID: "bid", Side: exchange.SideBuy, Price: 99},
			{ID: "ask", Side: exchange.SideSell, Price: 101},
		},
	}
	reg.IncFill(string(exchange.SideBuy))
	reg.IncPartialFills()
	reg.IncCancelCategory(cancelCategoryRiskTriggered)
	bot.maxQuoteAge = 8 * time.Second
	bot.maxAnchorDeviation = 12
	bot.maxNetInventory = 15

	summary := bot.Summary()
	if !summary.OpenBidPresent || !summary.OpenAskPresent {
		t.Fatal("expected open bid and ask present")
	}
	if summary.FillsBySide[string(exchange.SideBuy)] != 1 || summary.PartialFills != 1 {
		t.Fatalf("summary fills = %#v partials=%d", summary.FillsBySide, summary.PartialFills)
	}
	if summary.CancelCountsByCategory[cancelCategoryRiskTriggered] != 1 {
		t.Fatalf("cancel counts = %#v", summary.CancelCountsByCategory)
	}
	if summary.MaxObservedQuoteAge != 8*time.Second || summary.MaxObservedAnchorDeviation != 12 || summary.MaxObservedNetInventory != 15 {
		t.Fatalf("summary maxima = %#v", summary)
	}
}

func TestShutdownSummaryGeneration(t *testing.T) {
	reg := metrics.New()
	bot := NewBot(config.Config{}, &mockClient{}, exchange.MarketSpec{BaseAsset: "USDC", QuoteAsset: "cNGN"}, reg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	bot.haltCount = 2
	bot.persisted.LastHaltReason = "balances stale"
	bot.maxQuoteAge = 10 * time.Second
	bot.maxAnchorDeviation = 25
	bot.maxNetInventory = 42
	reg.IncFill(string(exchange.SideBuy))
	reg.IncCancelCategory(cancelCategoryKillSwitch)

	line := bot.ShutdownSummaryLine()
	for _, want := range []string{"halts=2", "balances stale", "fills_buy=1", "max_anchor_deviation_bps=25.0000", "max_net_inventory=42.000000"} {
		if !strings.Contains(line, want) {
			t.Fatalf("shutdown summary missing %q: %s", want, line)
		}
	}
}
