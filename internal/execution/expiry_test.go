package execution

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/metrics"
	"github.com/numofx/market-maker/internal/state"
	"github.com/numofx/market-maker/internal/strategy"
)

func expiryCfg() config.Config {
	return config.Config{
		CancelStaleOrderThreshold:  10,
		AdoptSizeTolerance:         0.000001,
		PostOnlyQuotes:             true,
		OrderExpirySeconds:         60,
		ExpiryReplaceMarginSeconds: 15,
	}
}

// Everything about the order matches its target -- price, size, terms -- so the expiry is the only
// thing that can be deciding.
func restingAtTarget(id string, expiry int64) exchange.Order {
	return exchange.Order{
		ID: id, Side: exchange.SideBuy, Price: 1326, Size: 1.2,
		CreatedAt: time.Now().UTC().Add(-45 * time.Second), PostOnly: true, WorstFee: "5000", Expiry: expiry,
	}
}

func TestEvaluateCancelReplacesAQuoteInsideItsExpiryMargin(t *testing.T) {
	now := time.Now().UTC()
	target := &strategy.Quote{Side: exchange.SideBuy, Price: 1326, Size: 1.2}
	cfg := expiryCfg()
	// A minimum lifetime far longer than the expiry: it must not hold an expiring quote back, or the
	// quote lapses while "too young" to replace.
	cfg.MinQuoteLifetime = 10 * time.Minute

	cases := []struct {
		name       string
		expiry     int64
		wantCancel bool
	}{
		{"inside the margin", now.Unix() + 10, true},
		{"exactly at the margin", now.Unix() + 15, true},
		{"already past expiry", now.Unix() - 5, true},
		{"outside the margin", now.Unix() + 40, false},
		// Unknown is not "expired". Replacing on a guess would churn every order whose row predates
		// the column.
		{"unknown expiry", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			current := restingAtTarget("o", tc.expiry)
			decision := evaluateCancel(&current, target, nil, cfg, time.Time{}, now, 0, "1000")
			if decision.Cancel != tc.wantCancel {
				t.Fatalf("cancel = %v (reason %q), want %v", decision.Cancel, decision.Reason, tc.wantCancel)
			}
			if decision.Suppress {
				t.Fatalf("suppressed (%s); an expiry replace is never suppressed", decision.SuppressReason)
			}
			if tc.wantCancel && (decision.Reason != cancelReasonExpiring || decision.EnforceRateLimit) {
				t.Fatalf("reason %q enforceRateLimit %v, want expiring and exempt", decision.Reason, decision.EnforceRateLimit)
			}
		})
	}
}

// The steady-state roll. With the cancel budget already spent, an expiring quote is still cancelled,
// its replacement is placed in the same Sync, the cancel does not consume budget, and it is counted
// under its own category rather than as replace-driven churn.
func TestExpiringQuoteIsReplacedInTheSameCycleWithoutSpendingTheCancelBudget(t *testing.T) {
	client := &mockClient{requiredWorstFee: "1000"}
	reg := metrics.New()
	cfg := expiryCfg()
	cfg.MaxCancelsPerMinute = 1
	syncer := NewSyncer(client, exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", MinSize: 0.000001}, cfg, reg,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	syncer.cancelTimestamps = []time.Time{time.Now().UTC().Add(-10 * time.Second)} // budget spent

	expiring := restingAtTarget("old", time.Now().UTC().Unix()+10)
	result, err := syncer.Sync(context.Background(),
		state.Snapshot{Market: "USDCcNGN-SPOT", OpenOrders: []exchange.Order{expiring}},
		strategy.Result{Bids: []strategy.Quote{{Side: exchange.SideBuy, Price: 1326, Size: 1.2}}},
		map[exchange.Side][]Identity{exchange.SideBuy: {{OrderID: "fresh", Nonce: "9"}}})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(client.cancelled) != 1 || client.cancelled[0] != "old" {
		t.Fatalf("cancelled %v, want the expiring quote replaced despite the spent budget", client.cancelled)
	}
	if len(client.placed) != 1 || client.placed[0].OrderID != "fresh" {
		t.Fatalf("placed %+v, want the replacement in the same cycle", client.placed)
	}
	if !result.Changed {
		t.Fatal("rolling a quote is a change")
	}
	if n := len(syncer.cancelTimestamps); n != 1 {
		t.Fatalf("cancel budget holds %d entries, want 1: an expiry roll must not spend budget", n)
	}

	rr := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rr.Body.String(), `category="expiry_replace"`) {
		t.Fatalf("metrics missing expiry_replace category:\n%s", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), `category="replace_driven"`) {
		t.Fatalf("an expiry roll was counted as replace_driven churn:\n%s", rr.Body.String())
	}
}

// Many expiring quotes in one cycle, a budget of one: all roll. Budget-limited churn would have
// stopped after the first and let the rest lapse.
func TestExpiringQuotesDoNotCountTowardTheRateLimit(t *testing.T) {
	client := &mockClient{requiredWorstFee: "1000"}
	cfg := expiryCfg()
	cfg.MaxCancelsPerMinute = 1
	syncer := NewSyncer(client, exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", MinSize: 0.000001}, cfg, metrics.New(),
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	soon := time.Now().UTC().Unix() + 5
	orders := []exchange.Order{restingAtTarget("a", soon), restingAtTarget("b", soon), restingAtTarget("c", soon)}
	orders[1].Price, orders[2].Price = 1325, 1324
	if _, err := syncer.Sync(context.Background(),
		state.Snapshot{Market: "USDCcNGN-SPOT", OpenOrders: orders},
		strategy.Result{Bids: []strategy.Quote{
			{Side: exchange.SideBuy, Price: 1326, Size: 1.2},
			{Side: exchange.SideBuy, Price: 1325, Size: 1.2},
			{Side: exchange.SideBuy, Price: 1324, Size: 1.2},
		}},
		map[exchange.Side][]Identity{exchange.SideBuy: {{OrderID: "n1", Nonce: "1"}, {OrderID: "n2", Nonce: "2"}, {OrderID: "n3", Nonce: "3"}}}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(client.cancelled) != 3 || len(client.placed) != 3 {
		t.Fatalf("cancelled %v placed %d, want all three rolled", client.cancelled, len(client.placed))
	}
	if got := syncer.cancelsPerMinute(); got != 0 {
		t.Fatalf("cancels per minute = %v, want 0", got)
	}
}

// The refresh throttle defers a cycle by up to MM_QUOTE_REFRESH_INTERVAL_MS. For a quote inside its
// expiry margin that is the difference between rolling and lapsing, so an expiring quote skips it.
func TestRunCycleSkipsTheRefreshThrottleForAnExpiringQuote(t *testing.T) {
	run := func(t *testing.T, bidExpiry int64) *integrationClient {
		t.Helper()
		now := time.Now().UTC()
		client := &integrationClient{
			spec: exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "USDC", QuoteAsset: "cNGN", TickSize: 0.01, SizeStep: 0.1, MinSize: 0.1},
			book: exchange.Book{Bids: []exchange.BookLevel{{Price: 99}}, Asks: []exchange.BookLevel{{Price: 101}}},
			balances: []exchange.Balance{
				{Asset: "USDC", Total: 50, Available: 50},
				{Asset: "cNGN", Total: 10000, Available: 10000},
			},
			mockClient: mockClient{openOrders: []exchange.Order{
				{ID: "mm:USDCcNGN-SPOT:buy:10", Side: exchange.SideBuy, Price: 99.9, Size: 10, Managed: true, Nonce: "10", CreatedAt: now.Add(-50 * time.Second), Expiry: bidExpiry},
				{ID: "mm:USDCcNGN-SPOT:sell:11", Side: exchange.SideSell, Price: 100.1, Size: 10, Managed: true, Nonce: "11", CreatedAt: now.Add(-5 * time.Second), Expiry: now.Unix() + 55},
			}},
		}
		cfg := config.Config{
			MarketSymbol: "USDCcNGN-SPOT", StateFile: filepath.Join(t.TempDir(), "state.json"),
			OrderSize: 10, HalfSpreadBPS: 10, MaxLongInventory: 100, MaxShortInventory: -100,
			CancelStaleOrderThreshold: 20, AdoptSizeTolerance: 0.01,
			QuoteRefreshInterval: time.Hour, OrderExpirySeconds: 60, ExpiryReplaceMarginSeconds: 15,
		}
		bot := NewBot(cfg, client, client.spec, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)), state.NewStore(cfg.StateFile))
		bot.snapshot.LastQuoteUpdate = now // quotes were just refreshed, so the throttle is armed
		if err := bot.RunCycle(context.Background()); err != nil {
			t.Fatalf("RunCycle: %v", err)
		}
		return client
	}

	t.Run("nothing expiring stays throttled", func(t *testing.T) {
		client := run(t, time.Now().UTC().Unix()+40)
		if len(client.cancelled) != 0 || len(client.placed) != 0 {
			t.Fatalf("cancelled %v placed %d; the throttle should have held", client.cancelled, len(client.placed))
		}
	})
	t.Run("an expiring bid rolls through the throttle", func(t *testing.T) {
		client := run(t, time.Now().UTC().Unix()+5)
		if len(client.cancelled) != 1 || client.cancelled[0] != "mm:USDCcNGN-SPOT:buy:10" {
			t.Fatalf("cancelled %v, want only the expiring bid", client.cancelled)
		}
		if len(client.placed) != 1 || client.placed[0].Side != exchange.SideBuy {
			t.Fatalf("placed %+v, want one replacement bid", client.placed)
		}
	})
}
