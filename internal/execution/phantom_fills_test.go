package execution

import (
	"context"
	"io"
	"log/slog"
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
