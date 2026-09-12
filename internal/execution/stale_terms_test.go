package execution

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/metrics"
	"github.com/numofx/market-maker/internal/state"
	"github.com/numofx/market-maker/internal/strategy"
)

func restingOrder(id string, side exchange.Side, postOnly bool, worstFee string) exchange.Order {
	price := 99.9
	if side == exchange.SideSell {
		price = 100.1
	}
	return exchange.Order{
		ID: "mm:USDCcNGN-SPOT:" + string(side) + ":" + id, Market: "USDCcNGN-SPOT",
		Side: side, Price: price, Size: 10, Nonce: id, Managed: true,
		CreatedAt: time.Now().UTC().Add(-time.Hour),
		PostOnly:  postOnly, WorstFee: worstFee,
	}
}

// staleTermsClient is priced and funded, so risk does not halt and the startup terms gate is
// actually reached. Without the book and balances every order is rejected as
// "risk_halt:reference price unavailable" -- which looks like a pass to any assertion phrased as
// "not rejected for reason X", and the first version of these tests fell for exactly that.
func staleTermsClient(orders []exchange.Order, requiredWorstFee string) *integrationClient {
	return &integrationClient{
		spec: exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "USDC", QuoteAsset: "cNGN", TickSize: 0.01, SizeStep: 0.1, MinSize: 0.1},
		book: exchange.Book{Bids: []exchange.BookLevel{{Price: 99}}, Asks: []exchange.BookLevel{{Price: 101}}},
		balances: []exchange.Balance{
			{Asset: "USDC", Total: 50, Available: 50},
			{Asset: "cNGN", Total: 10000, Available: 10000},
		},
		mockClient: mockClient{openOrders: orders, requiredWorstFee: requiredWorstFee},
	}
}

func staleTermsRun(t *testing.T, client *integrationClient, postOnly bool) ReconciliationResult {
	t.Helper()
	cfg := config.Config{
		MarketSymbol: "USDCcNGN-SPOT", StateFile: filepath.Join(t.TempDir(), "state.json"),
		OrderSize: 10, HalfSpreadBPS: 10, MaxLongInventory: 100, MaxShortInventory: -100,
		CancelStaleOrderThreshold: 20, AdoptSizeTolerance: 0.01, PostOnlyQuotes: postOnly,
	}
	bot := NewBot(cfg, client, client.spec, metrics.New(),
		slog.New(slog.NewTextHandler(io.Discard, nil)), state.NewStore(cfg.StateFile))
	if err := bot.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	return bot.lastReconciliation
}

// The exact production case. Post-only shipped, the bot restarted, and the previous image's quotes
// were still resting without the flag -- and because the ladder was stable, nothing ever replaced
// them. Twenty minutes of live quotes that could take, while the config said they could not.
func TestStartupReplacesOrdersMissingPostOnly(t *testing.T) {
	orders := []exchange.Order{
		restingOrder("1", exchange.SideBuy, false, "9999999999999"),
		restingOrder("2", exchange.SideSell, false, "9999999999999"),
	}
	client := staleTermsClient(orders, "1000")
	result := staleTermsRun(t, client, true)

	for _, o := range orders {
		if reason := result.RejectedReasons[o.ID]; reason != "post_only_mismatch" {
			t.Fatalf("order %s rejected for %q, want post_only_mismatch", o.ID, reason)
		}
	}
	if len(client.cancelled) != 2 {
		t.Fatalf("cancelled %v, want both stale-terms orders off the book", client.cancelled)
	}
}

// A bound signed under an older, lower fee schedule reverts TM_FeeTooHigh on every fill it takes.
// It goes for the same reason: signed terms cannot be amended in place.
func TestStartupReplacesOrdersWhoseBoundNoLongerCoversTheSchedule(t *testing.T) {
	client := staleTermsClient([]exchange.Order{restingOrder("1", exchange.SideBuy, true, "1000")}, "2000")
	result := staleTermsRun(t, client, true)

	if reason := result.RejectedReasons["mm:USDCcNGN-SPOT:buy:1"]; reason != "worst_fee_below_current_schedule" {
		t.Fatalf("rejected for %q, want worst_fee_below_current_schedule", reason)
	}
	if len(client.cancelled) != 1 {
		t.Fatalf("cancelled %v, want the stale-bound order removed", client.cancelled)
	}
}

// A bound ABOVE what is required is fine -- headroom is deliberate, and churning for it would
// replace every order on every restart. Asserted by what happened to the order, not by the absence
// of one particular reason.
func TestStartupKeepsOrdersWithHeadroomAboveTheSchedule(t *testing.T) {
	client := staleTermsClient([]exchange.Order{restingOrder("1", exchange.SideBuy, true, "5000")}, "2000")
	result := staleTermsRun(t, client, true)

	if reason := result.RejectedReasons["mm:USDCcNGN-SPOT:buy:1"]; reason != "" {
		t.Fatalf("an order with headroom was rejected for %q", reason)
	}
	if len(client.cancelled) != 0 {
		t.Fatalf("cancelled %v, want the order left resting", client.cancelled)
	}
}

// The check is "matches config", not "has the flag". Turning post-only off must also replace the
// old ladder, or the operator has revoked terms that are still in force on the book.
func TestStartupReplacesPostOnlyOrdersWhenConfigTurnsItOff(t *testing.T) {
	client := staleTermsClient([]exchange.Order{restingOrder("1", exchange.SideBuy, true, "9999999999999")}, "1000")
	result := staleTermsRun(t, client, false)

	if reason := result.RejectedReasons["mm:USDCcNGN-SPOT:buy:1"]; reason != "post_only_mismatch" {
		t.Fatalf("rejected for %q, want post_only_mismatch", reason)
	}
}

// A bound we cannot parse is a row we cannot reason about. Replaced explicitly rather than assumed
// safe.
func TestStartupReplacesAnUnreadableBound(t *testing.T) {
	client := staleTermsClient([]exchange.Order{restingOrder("1", exchange.SideBuy, true, "not-a-number")}, "2000")
	result := staleTermsRun(t, client, true)

	if reason := result.RejectedReasons["mm:USDCcNGN-SPOT:buy:1"]; reason != "unreadable_worst_fee" {
		t.Fatalf("rejected for %q, want unreadable_worst_fee", reason)
	}
}

// When the required bound cannot be determined, leave the book alone rather than churning it on a
// guess -- the schedule refresh forces a requote if the fee actually moved.
func TestStartupKeepsOrdersWhenTheRequiredBoundIsUnknown(t *testing.T) {
	client := staleTermsClient([]exchange.Order{restingOrder("1", exchange.SideBuy, true, "1")}, "")
	result := staleTermsRun(t, client, true)

	if reason := result.RejectedReasons["mm:USDCcNGN-SPOT:buy:1"]; reason != "" {
		t.Fatalf("rejected for %q with no schedule to compare against", reason)
	}
	if len(client.cancelled) != 0 {
		t.Fatalf("cancelled %v on an unknown bound", client.cancelled)
	}
}

// The hole the drill exposed. ECS drains the previous task AFTER the new one is healthy, so a
// predecessor keeps quoting and can place orders seconds after its successor's startup
// reconciliation has already run. Startup never sees them, and on a stable ladder nothing else
// ever replaces them -- the live book held six non-post-only maker quotes against a post-only
// config for exactly this reason.
//
// Driven through Sync, not through startup, because the whole point is that startup is over.
func TestSteadyStateCycleReplacesAPredecessorsStaleQuote(t *testing.T) {
	client := &mockClient{requiredWorstFee: "1000"}
	syncer := NewSyncer(client, exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", MinSize: 0.000001},
		config.Config{CancelStaleOrderThreshold: 10, AdoptSizeTolerance: 0.000001, PostOnlyQuotes: true},
		metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Price and size match the target exactly, so every other check would keep it. Only the terms
	// differ -- which is what a draining predecessor leaves behind.
	stale := exchange.Order{
		ID: "left-behind", Side: exchange.SideBuy, Price: 1326, Size: 1.2,
		CreatedAt: time.Now().UTC().Add(-time.Minute), PostOnly: false, WorstFee: "9999999999",
	}

	result, err := syncer.Sync(context.Background(),
		state.Snapshot{Market: "USDCcNGN-SPOT", OpenOrders: []exchange.Order{stale}},
		strategy.Result{Bids: []strategy.Quote{{Side: exchange.SideBuy, Price: 1326, Size: 1.2}}},
		map[exchange.Side][]Identity{exchange.SideBuy: {{OrderID: "fresh", Nonce: "9"}}})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(client.cancelled) != 1 || client.cancelled[0] != "left-behind" {
		t.Fatalf("cancelled %v, want the stale-terms quote replaced mid-cycle", client.cancelled)
	}
	if len(client.placed) != 1 || !client.placed[0].PostOnly {
		t.Fatalf("placed %+v, want one post-only replacement", client.placed)
	}
	if !result.Changed {
		t.Fatal("replacing a quote is a change")
	}
}

// A compliant quote at the right price and size is left alone -- otherwise the check would churn
// the entire ladder on every cycle, which is worse than the problem it fixes.
func TestSteadyStateCycleKeepsACompliantQuote(t *testing.T) {
	client := &mockClient{requiredWorstFee: "1000"}
	syncer := NewSyncer(client, exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", MinSize: 0.000001},
		config.Config{CancelStaleOrderThreshold: 10, AdoptSizeTolerance: 0.000001, PostOnlyQuotes: true},
		metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	ok := exchange.Order{
		ID: "compliant", Side: exchange.SideBuy, Price: 1326, Size: 1.2,
		CreatedAt: time.Now().UTC().Add(-time.Minute), PostOnly: true, WorstFee: "5000",
	}

	if _, err := syncer.Sync(context.Background(),
		state.Snapshot{Market: "USDCcNGN-SPOT", OpenOrders: []exchange.Order{ok}},
		strategy.Result{Bids: []strategy.Quote{{Side: exchange.SideBuy, Price: 1326, Size: 1.2}}},
		map[exchange.Side][]Identity{exchange.SideBuy: {{OrderID: "fresh", Nonce: "9"}}}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(client.cancelled) != 0 {
		t.Fatalf("cancelled %v, want a compliant quote left resting", client.cancelled)
	}
}

// Stale terms must not be deferred when the cancel budget is spent. The budget bounds churn; an
// order that can take when the operator said it must not is not churn to be smoothed out.
func TestStaleTermsAreNotDeferredByTheCancelBudget(t *testing.T) {
	client := &mockClient{requiredWorstFee: "1000"}
	syncer := NewSyncer(client, exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", MinSize: 0.000001},
		config.Config{CancelStaleOrderThreshold: 10, AdoptSizeTolerance: 0.000001,
			PostOnlyQuotes: true, MaxCancelsPerMinute: 1},
		metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	syncer.cancelTimestamps = []time.Time{time.Now().UTC().Add(-10 * time.Second)} // budget spent

	stale := exchange.Order{
		ID: "left-behind", Side: exchange.SideBuy, Price: 1326, Size: 1.2,
		CreatedAt: time.Now().UTC().Add(-time.Minute), PostOnly: false, WorstFee: "9999999999",
	}

	if _, err := syncer.Sync(context.Background(),
		state.Snapshot{Market: "USDCcNGN-SPOT", OpenOrders: []exchange.Order{stale}},
		strategy.Result{Bids: []strategy.Quote{{Side: exchange.SideBuy, Price: 1326, Size: 1.2}}},
		map[exchange.Side][]Identity{exchange.SideBuy: {{OrderID: "fresh", Nonce: "9"}}}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(client.cancelled) != 1 {
		t.Fatalf("cancelled %v with the budget spent; stale terms must not be deferred", client.cancelled)
	}
}
