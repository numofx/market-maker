package execution

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/metrics"
	"github.com/numofx/market-maker/internal/state"
)

// marginClient is a venue with the cNGN future's margin: why an order left the book, and the account's
// IM headroom on the engine.
type marginClient struct {
	integrationClient
	reasons     map[string]string
	surplus     float64
	perContract float64
	headroomErr error
}

func (c *marginClient) CancelReasons(_ context.Context, ids []string) (map[string]string, error) {
	out := map[string]string{}
	for _, id := range ids {
		if reason, ok := c.reasons[id]; ok {
			out[id] = reason
		}
	}
	return out, nil
}

func (c *marginClient) FutureIMHeadroom(context.Context, exchange.MarketSpec) (float64, float64, error) {
	return c.surplus, c.perContract, c.headroomErr
}

func marginBot(t *testing.T) (*Bot, *marginClient, *metrics.Registry) {
	t.Helper()
	client := &marginClient{
		integrationClient: integrationClient{
			spec: exchange.MarketSpec{Symbol: "USDC-cNGN-31DEC26", BaseAsset: "USDC", QuoteAsset: "cNGN", TickSize: 0.01, SizeStep: 0.001, MinSize: 0.001,
				Kind: exchange.MarketKindFuture, MarginManager: "0x00000000000000000000000000000000000000e9", ContractSizeUSDC: 1000},
			book: exchange.Book{Bids: []exchange.BookLevel{{Price: 1399}}, Asks: []exchange.BookLevel{{Price: 1401}}},
			balances: []exchange.Balance{
				{Asset: "USDC", Total: 0, Available: 0},
				{Asset: "cNGN", Total: 10_000_000, Available: 10_000_000},
			},
		},
		reasons: map[string]string{},
		// 25% IM on 1,000 USDC: 250 USD a contract; 10 contracts need 2,500
		perContract: 250,
	}
	cfg := config.Config{
		MarketSymbol: "USDC-cNGN-31DEC26", StateFile: filepath.Join(t.TempDir(), "state.json"),
		OrderSize: 10, HalfSpreadBPS: 10, MaxLongInventory: 100, MaxShortInventory: -100,
		QuoteRefreshInterval: 0, CancelStaleOrderThreshold: 20, AdoptSizeTolerance: 0.01,
	}
	registry := metrics.New()
	bot := NewBot(cfg, client, client.spec, registry, slog.New(slog.NewTextHandler(io.Discard, nil)), state.NewStore(cfg.StateFile))
	return bot, client, registry
}

func placedSides(placed []exchange.PlaceOrderRequest) map[exchange.Side]int {
	out := map[exchange.Side]int{}
	for _, p := range placed {
		out[p.Side]++
	}
	return out
}

func restingFrom(placed exchange.PlaceOrderRequest) exchange.Order {
	return exchange.Order{ID: placed.OrderID, Side: placed.Side, Nonce: placed.Nonce, Price: placed.Price, Size: placed.Size, Managed: true}
}

// After the venue cancels a quote for insufficient margin, that side stays off the book while the
// account is short -- requoting it would only rest an order the venue cancels again a minute later --
// and is requoted on the first cycle the account can carry it. The other side quotes throughout, and
// the cancel is not counted as a fill.
func TestAfterAnInsufficientMarginCancelTheSideRequotesOnceMarginReturns(t *testing.T) {
	bot, client, registry := marginBot(t)
	ctx := context.Background()
	client.surplus = 1_000_000

	if err := bot.RunCycle(ctx); err != nil {
		t.Fatal(err)
	}
	sides := placedSides(client.placed)
	if sides[exchange.SideBuy] != 1 || sides[exchange.SideSell] != 1 {
		t.Fatalf("cycle 1 quotes both sides: %v", sides)
	}
	var bid, ask exchange.PlaceOrderRequest
	for _, p := range client.placed {
		if p.Side == exchange.SideBuy {
			bid = p
		} else {
			ask = p
		}
	}
	client.openOrders = []exchange.Order{restingFrom(bid), restingFrom(ask)}
	if err := bot.RunCycle(ctx); err != nil {
		t.Fatal(err)
	}

	// The venue cancels the ask: the account cannot carry it.
	client.surplus = 10
	client.openOrders = []exchange.Order{restingFrom(bid)}
	client.reasons[ask.OrderID] = exchange.CancelReasonInsufficientMargin
	before := len(client.placed)
	for i := 0; i < 3; i++ {
		if err := bot.RunCycle(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if sells := placedSides(client.placed[before:])[exchange.SideSell]; sells != 0 {
		t.Fatalf("the ask was requoted %d times while the account was still short", sells)
	}
	if bot.lastQuotes.AskSuppression == nil || bot.lastQuotes.AskSuppression.Reason != exchange.CancelReasonInsufficientMargin {
		t.Fatalf("the ask's suppression must say why: %+v", bot.lastQuotes.AskSuppression)
	}
	if fills, _, _ := registry.SnapshotCounters(); fills[string(exchange.SideSell)] != 0 {
		t.Fatalf("a venue cancel is not a fill: %v", fills)
	}

	// Short of one order's IM by a dollar: still held.
	client.surplus = 2_499
	if err := bot.RunCycle(ctx); err != nil {
		t.Fatal(err)
	}
	if sells := placedSides(client.placed[before:])[exchange.SideSell]; sells != 0 {
		t.Fatal("requoted with less IM headroom than one order needs")
	}

	// Topped up: the ask is back on the next cycle.
	client.surplus = 2_500
	if err := bot.RunCycle(ctx); err != nil {
		t.Fatal(err)
	}
	if sells := placedSides(client.placed[before:])[exchange.SideSell]; sells != 1 {
		t.Fatalf("the ask must be requoted once margin returns: %d", sells)
	}
	if len(bot.marginHolds) != 0 {
		t.Fatalf("the hold is released: %v", bot.marginHolds)
	}
}

// A hold whose headroom cannot be read is not forever: past marginHoldMax the side is requoted and
// the venue's own check decides.
func TestAnUnreadableMarginHoldIsReleasedAfterItsMaximum(t *testing.T) {
	bot, client, _ := marginBot(t)
	ctx := context.Background()
	client.headroomErr = errors.New("rpc down")
	bot.marginHolds = map[exchange.Side]time.Time{exchange.SideSell: time.Now().UTC()}
	if err := bot.RunCycle(ctx); err != nil {
		t.Fatal(err)
	}
	if placedSides(client.placed)[exchange.SideSell] != 0 {
		t.Fatal("an unreadable hold inside its maximum keeps the side off")
	}
	bot.marginHolds[exchange.SideSell] = time.Now().UTC().Add(-marginHoldMax)
	if err := bot.RunCycle(ctx); err != nil {
		t.Fatal(err)
	}
	if placedSides(client.placed)[exchange.SideSell] != 1 {
		t.Fatal("past its maximum the side is requoted")
	}
}

// Any other venue cancel (reduce_only_no_position, a cancel by an operator) is not a fill either, and
// holds nothing.
func TestAnyVenueCancelIsNotAFillAndOnlyMarginHolds(t *testing.T) {
	bot, client, registry := marginBot(t)
	ctx := context.Background()
	client.surplus = 1_000_000
	if err := bot.RunCycle(ctx); err != nil {
		t.Fatal(err)
	}
	orders := []exchange.Order{}
	var ask exchange.PlaceOrderRequest
	for _, p := range client.placed {
		orders = append(orders, restingFrom(p))
		if p.Side == exchange.SideSell {
			ask = p
		}
	}
	client.openOrders = orders
	if err := bot.RunCycle(ctx); err != nil {
		t.Fatal(err)
	}
	client.openOrders = orders[:0]
	for _, o := range orders {
		if o.Side == exchange.SideBuy {
			client.openOrders = append(client.openOrders, o)
		}
	}
	client.reasons[ask.OrderID] = "reduce_only_no_position"
	before := len(client.placed)
	if err := bot.RunCycle(ctx); err != nil {
		t.Fatal(err)
	}
	if fills, _, _ := registry.SnapshotCounters(); fills[string(exchange.SideSell)] != 0 {
		t.Fatalf("a venue cancel is not a fill: %v", fills)
	}
	if len(bot.marginHolds) != 0 || placedSides(client.placed[before:])[exchange.SideSell] != 1 {
		t.Fatal("a cancel for any other reason holds nothing: the side is requoted")
	}
}
