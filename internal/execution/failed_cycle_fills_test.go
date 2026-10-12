package execution

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/metrics"
	"github.com/numofx/market-maker/internal/state"
)

// The perp bot on 2026-10-01..05 (task 890da33b) counted 4,382 fills against 11 real ones, through 883
// failed placements (POST /v1/orders 503). A failing cycle returns before saving its snapshot, so every
// cycle of a failing streak compared against the last good one -- in which a quote cancelled early in the
// streak still rested -- with that cancel long since drained, and counted it again, once per cycle.
func TestOrdersCancelledInAFailedCycleAreNotFills(t *testing.T) {
	client := &integrationClient{
		spec: exchange.MarketSpec{Symbol: "USDCcNGN-APR30-2026", BaseAsset: "USDC", QuoteAsset: "cNGN", TickSize: 0.01, SizeStep: 0.1, MinSize: 0.1},
		book: exchange.Book{Bids: []exchange.BookLevel{{Price: 0.99}}, Asks: []exchange.BookLevel{{Price: 1.01}}},
		balances: []exchange.Balance{
			{Asset: "USDC", Total: 0, Available: 0},
			{Asset: "cNGN", Total: 100000, Available: 100000},
		},
	}
	cfg := config.Config{
		MarketSymbol: "USDCcNGN-APR30-2026", StateFile: filepath.Join(t.TempDir(), "state.json"),
		OrderSize: 10, HalfSpreadBPS: 10, MaxLongInventory: 100, MaxShortInventory: -100,
		QuoteRefreshInterval: 0, CancelStaleOrderThreshold: 20, AdoptSizeTolerance: 0.01,
		MaxCancelsPerMinute: 1000,
	}
	registry := metrics.New()
	bot := NewBot(cfg, client, client.spec, registry, slog.New(slog.NewTextHandler(io.Discard, nil)), state.NewStore(cfg.StateFile))
	ctx := context.Background()

	// cycle 1 places a bid and an ask; they rest
	if err := bot.RunCycle(ctx); err != nil {
		t.Fatal(err)
	}
	for _, p := range client.placed {
		client.openOrders = append(client.openOrders, exchange.Order{ID: p.OrderID, Side: p.Side, Nonce: p.Nonce, Price: p.Price, Size: p.Size, Managed: true})
	}
	if err := bot.RunCycle(ctx); err != nil {
		t.Fatal(err)
	}

	// the market moves: the bot cancels both quotes to replace them, and the venue refuses the placement
	client.book = exchange.Book{Bids: []exchange.BookLevel{{Price: 1.09}}, Asks: []exchange.BookLevel{{Price: 1.11}}}
	client.placeErr = errors.New("POST /v1/orders returned 503")
	cancelledBefore := len(client.cancelled)
	if err := bot.RunCycle(ctx); err == nil {
		t.Fatal("the cycle must fail on the refused placement")
	}
	if len(client.cancelled) == cancelledBefore {
		t.Fatal("the failing cycle must have cancelled the resting quotes first")
	}
	// the cancels landed: those orders are gone from the venue; anything not cancelled still rests
	gone := map[string]bool{}
	for _, id := range client.cancelled[cancelledBefore:] {
		gone[id] = true
	}
	var still []exchange.Order
	for _, o := range client.openOrders {
		if !gone[o.ID] {
			still = append(still, o)
		}
	}
	client.openOrders = still
	// the venue keeps refusing for a while: every one of these cycles compared against the last good
	// snapshot, in which the cancelled quote still rested, and counted it again
	for i := 0; i < 5; i++ {
		if err := bot.RunCycle(ctx); err == nil {
			t.Fatal("the venue is still refusing placements")
		}
	}
	client.placeErr = nil
	if err := bot.RunCycle(ctx); err != nil {
		t.Fatal(err)
	}

	if fills, _, _ := registry.SnapshotCounters(); fills[string(exchange.SideBuy)]+fills[string(exchange.SideSell)] != 0 {
		t.Fatalf("orders the bot cancelled were counted as fills: %v", fills)
	}
}
