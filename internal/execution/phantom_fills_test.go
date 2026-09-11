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

func snap(orders ...exchange.Order) state.Snapshot {
	return state.Snapshot{Market: "USDCcNGN-SPOT", OpenOrders: orders}
}

func resting(id string, side exchange.Side, size float64) exchange.Order {
	return exchange.Order{ID: id, Side: side, Price: 1326, Size: size,
		CreatedAt: time.Now().UTC().Add(-time.Minute)}
}

// The bug: an order the bot cancelled itself disappears exactly like a filled one, and was counted
// as a fill. With the ladder replacing on every poll this reported thousands of fills on a venue
// that had seen eight trades in its entire history.
func TestACancelledOrderIsNotAFill(t *testing.T) {
	before := snap(resting("b1", exchange.SideBuy, 1.2))
	after := snap()
	cancelled := map[string]struct{}{"b1": {}}

	fills, partials, ok := observeOrderStateFills(before, after, cancelled)
	if !ok {
		t.Fatal("the comparison is still authoritative -- we know exactly what happened to b1")
	}
	if len(fills) != 0 {
		t.Fatalf("counted %v as fills; the bot cancelled that order itself", fills)
	}
	if partials != 0 {
		t.Fatalf("partials = %d, want 0", partials)
	}
}

// ...and the counter must still do its actual job.
func TestAnOrderThatVanishedWithoutBeingCancelledIsAFill(t *testing.T) {
	before := snap(resting("b1", exchange.SideBuy, 1.2))
	after := snap()

	fills, _, ok := observeOrderStateFills(before, after, map[string]struct{}{})
	if !ok || fills[string(exchange.SideBuy)] != 1 {
		t.Fatalf("fills = %v, want one buy fill when nothing we did explains the disappearance", fills)
	}
}

// A partial fill shrinks an order rather than removing it, and is unaffected by any of this. This
// path was already correct -- it recorded the one real fill the venue has seen since fees went
// live, while the disappearance path was inventing thousands.
func TestAPartialFillIsStillCounted(t *testing.T) {
	before := snap(resting("b1", exchange.SideBuy, 1.2))
	after := snap(resting("b1", exchange.SideBuy, 0.9))

	fills, partials, ok := observeOrderStateFills(before, after, map[string]struct{}{"b1": {}})
	if !ok {
		t.Fatal("a shrinking order is authoritative")
	}
	if fills[string(exchange.SideBuy)] != 1 || partials != 1 {
		t.Fatalf("fills = %v partials = %d; a partial fill is a fill even on an order we later cancel",
			fills, partials)
	}
}

// The set has to cover exactly the interval between the two snapshots. A stale id would suppress a
// genuine fill later; draining on read is what prevents that.
func TestTakeCancelledDrains(t *testing.T) {
	s := NewSyncer(&mockClient{}, exchange.MarketSpec{Symbol: "USDCcNGN-SPOT"},
		config.Config{}, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	s.noteCancelled("a")
	s.noteCancelled("b")
	if got := s.TakeCancelled(); len(got) != 2 {
		t.Fatalf("took %v, want both ids", got)
	}
	if got := s.TakeCancelled(); len(got) != 0 {
		t.Fatalf("took %v on the second call, want an empty set", got)
	}
}

// End to end: a replace-driven cancel must leave the fill counters alone. This is the production
// shape -- the ladder churning and reporting fills that never happened.
func TestAReplaceDoesNotRegisterAsAFill(t *testing.T) {
	client := &mockClient{openOrders: []exchange.Order{resting("b1", exchange.SideBuy, 1.2)}}
	syncer := NewSyncer(client, exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", MinSize: 0.000001},
		config.Config{CancelStaleOrderThreshold: 10, AdoptSizeTolerance: 0.000001},
		metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	// A price move large enough to force a genuine replace.
	if _, err := syncer.Sync(context.Background(), snap(resting("b1", exchange.SideBuy, 1.2)),
		strategy.Result{Bids: []strategy.Quote{{Side: exchange.SideBuy, Price: 1400, Size: 1.2}}},
		map[exchange.Side][]Identity{exchange.SideBuy: {{OrderID: "b2", Nonce: "2"}}}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(client.cancelled) == 0 {
		t.Fatal("the premise needs a real cancel; nothing was cancelled")
	}

	// The order is gone from the book next cycle because we replaced it, not because it traded.
	fills, _, _ := observeOrderStateFills(
		snap(resting("b1", exchange.SideBuy, 1.2)), snap(), syncer.TakeCancelled())
	if len(fills) != 0 {
		t.Fatalf("a replace reported %v as fills", fills)
	}
}

// The startup path cancels through the client directly rather than through the syncer, so it was
// missed by the first pass at this: a restart cancelled the previous container's ladder, the next
// comparison saw those orders vanish with nothing to attribute them to, and counted them as fills.
// Bounded at one ladder per boot rather than thousands per hour, but it meant the counter never
// started at zero -- which is the whole point of a fill counter.
//
// This drives Initialize rather than calling noteCancelled directly, because the defect was in the
// WIRING, not the mechanism. A test that pokes the set by hand passes whether or not the bot ever
// feeds it, which is exactly the trap the first version of this test fell into.
func TestStartupCancelsAreNotCountedAsFills(t *testing.T) {
	before := []exchange.Order{
		{ID: "mm:USDCcNGN-SPOT:buy:1", Side: exchange.SideBuy, Nonce: "1", Managed: true},
		{ID: "manual-order", Side: exchange.SideSell, Nonce: "2", Managed: false},
	}
	client := &integrationClient{
		spec:       exchange.MarketSpec{Symbol: "USDCcNGN-APR30-2026", BaseAsset: "USDC", QuoteAsset: "cNGN", TickSize: 0.01, SizeStep: 0.1, MinSize: 0.1},
		mockClient: mockClient{openOrders: before},
	}
	cfg := config.Config{MarketSymbol: "USDCcNGN-SPOT", StateFile: filepath.Join(t.TempDir(), "state.json")}
	bot := NewBot(cfg, client, client.spec, metrics.New(),
		slog.New(slog.NewTextHandler(io.Discard, nil)), state.NewStore(cfg.StateFile))

	if err := bot.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if len(client.cancelled) == 0 {
		t.Fatal("the premise needs startup to cancel something; nothing was cancelled")
	}

	// The very next observation compares the pre-restart book against an empty one.
	fills, partials, _ := observeOrderStateFills(
		snap(before...), snap(), bot.syncer.TakeCancelled())
	if len(fills) != 0 || partials != 0 {
		t.Fatalf("a restart reported %v fills (%d partial); the counter must start at zero", fills, partials)
	}
}

// A restart that adopts rather than cancels must still report a real fill that happened while the
// bot was down -- the guard is about attribution, not about suppressing everything at boot.
func TestARestartStillSeesAFillItDidNotCause(t *testing.T) {
	previous := snap(resting("b1", exchange.SideBuy, 1.2), resting("s1", exchange.SideSell, 1.2))

	s := NewSyncer(&mockClient{}, exchange.MarketSpec{Symbol: "USDCcNGN-SPOT"},
		config.Config{}, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.noteCancelled("b1") // we cancelled this one

	// s1 is gone and we never touched it: it traded.
	fills, _, _ := observeOrderStateFills(previous, snap(), s.TakeCancelled())
	if fills[string(exchange.SideSell)] != 1 {
		t.Fatalf("fills = %v, want the untouched order counted as a sell fill", fills)
	}
	if fills[string(exchange.SideBuy)] != 0 {
		t.Fatalf("fills = %v, want the cancelled order ignored", fills)
	}
}
