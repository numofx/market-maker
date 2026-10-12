package execution

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/metrics"
	"github.com/numofx/market-maker/internal/state"
)

// engineFutureClient is a venue with the cNGN future under the MarginEngine: its chain state, and why an
// order left the book.
type engineFutureClient struct {
	integrationClient
	reasons  map[string]string
	future   exchange.FutureState
	stateErr error
}

func (c *engineFutureClient) CancelReasons(_ context.Context, ids []string) (map[string]string, error) {
	out := map[string]string{}
	for _, id := range ids {
		if reason, ok := c.reasons[id]; ok {
			out[id] = reason
		}
	}
	return out, nil
}

func (c *engineFutureClient) FutureState(context.Context, exchange.MarketSpec) (exchange.FutureState, error) {
	return c.future, c.stateErr
}

// One contract is 1,000 USDC at a 25% IM: 250 USD of IM a contract. The index is 1,400 cNGN per USDC with
// a 10% band; fixing is a week out. The bot quotes 10 contracts a side when it can carry them.
func engineFutureBot(t *testing.T) (*Bot, *engineFutureClient, *metrics.Registry) {
	t.Helper()
	now := time.Now().UTC()
	client := &engineFutureClient{
		integrationClient: integrationClient{
			spec: exchange.MarketSpec{Symbol: "USDC-cNGN-31DEC26", BaseAsset: "USDC", QuoteAsset: "cNGN", TickSize: 0.000001, SizeStep: 0.001, MinSize: 0.001,
				Kind: exchange.MarketKindFuture, MarginManager: "0x00000000000000000000000000000000000000e9", ContractSizeUSDC: 1000, TakerFeeBps: 25,
				ExpiryTimestamp: now.Add(8 * 24 * time.Hour).Unix()},
			book: exchange.Book{Bids: []exchange.BookLevel{{Price: 1399}}, Asks: []exchange.BookLevel{{Price: 1401}}},
			// cash is not the future's budget: deliberately none
			balances: []exchange.Balance{{Asset: "USDC", Total: 0, Available: 0}, {Asset: "cNGN", Total: 0, Available: 0}},
		},
		reasons: map[string]string{},
		future: exchange.FutureState{IMSurplus: 1_000_000, PerContractIM: 250, Index: 1400, BandWidth: 0.1, BandLow: 1260, BandHigh: 1540,
			FixingStart: now.Add(7 * 24 * time.Hour), LastTradeTime: now.Add(7*24*time.Hour + time.Hour)},
	}
	cfg := config.Config{
		MarketSymbol: "USDC-cNGN-31DEC26", StateFile: filepath.Join(t.TempDir(), "state.json"),
		OrderSize: 10, HalfSpreadBPS: 10, MaxLongInventory: 100, MaxShortInventory: -100,
		QuoteRefreshInterval: 0, CancelStaleOrderThreshold: 20, AdoptSizeTolerance: 0.01, MaxCancelsPerMinute: 1000,
		FutureIMUsage: 0.8, FutureBandBufferBPS: 10, FutureCutoffBeforeFixSeconds: 3600,
		AnchorSourceType: "future_index_carry", CarryRateAPR: 0.2,
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

func lastPlaced(placed []exchange.PlaceOrderRequest, side exchange.Side) *exchange.PlaceOrderRequest {
	for i := len(placed) - 1; i >= 0; i-- {
		if placed[i].Side == side {
			return &placed[i]
		}
	}
	return nil
}

// The bot starts on the MarginEngine future and quotes it from its IM, with no cash at all: both sides,
// 10 contracts, inside the band.
func TestTheBotQuotesTheEngineFutureFromItsIM(t *testing.T) {
	bot, client, _ := engineFutureBot(t)
	ctx := context.Background()
	if err := bot.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := bot.RunCycle(ctx); err != nil {
		t.Fatal(err)
	}
	bid, ask := lastPlaced(client.placed, exchange.SideBuy), lastPlaced(client.placed, exchange.SideSell)
	if bid == nil || ask == nil || bid.Size != 10 || ask.Size != 10 {
		t.Fatalf("both sides, 10 contracts: %+v %+v", bid, ask)
	}
	if bid.Price < 1260 || ask.Price > 1540 || bid.Price >= ask.Price {
		t.Fatalf("inside the band: %v / %v", bid.Price, ask.Price)
	}
}

// After the venue cancels a quote for insufficient margin, that side is not requoted while the account
// cannot carry an order, and is requoted -- at what it can carry -- on the first cycle it can. The cancel
// is not counted as a fill.
func TestAfterAnInsufficientMarginCancelTheSideRequotesOnceMarginReturns(t *testing.T) {
	bot, client, registry := engineFutureBot(t)
	ctx := context.Background()
	if err := bot.RunCycle(ctx); err != nil {
		t.Fatal(err)
	}
	bid, ask := *lastPlaced(client.placed, exchange.SideBuy), *lastPlaced(client.placed, exchange.SideSell)
	client.openOrders = []exchange.Order{restingFrom(bid), restingFrom(ask)}
	if err := bot.RunCycle(ctx); err != nil {
		t.Fatal(err)
	}

	// The account's IM collapses (a mark moved against it) and the venue cancels the ask.
	client.future.IMSurplus = 0.1 // 0.0003 contracts of headroom: under the 0.001 minimum
	client.openOrders = []exchange.Order{restingFrom(bid)}
	client.reasons[ask.OrderID] = exchange.CancelReasonInsufficientMargin
	before := len(client.placed)
	for i := 0; i < 3; i++ {
		if err := bot.RunCycle(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if sells := placedSides(client.placed[before:])[exchange.SideSell]; sells != 0 {
		t.Fatalf("the ask was requoted %d times while the account could not carry it", sells)
	}
	if fills, _, _ := registry.SnapshotCounters(); fills[string(exchange.SideSell)] != 0 {
		t.Fatalf("a venue cancel is not a fill: %v", fills)
	}

	// Topped up to 2,000 USD: 2,000 x 0.8 / 250 = 6.4 contracts. The ask is back, at that size.
	client.future.IMSurplus = 2000
	if err := bot.RunCycle(ctx); err != nil {
		t.Fatal(err)
	}
	requoted := lastPlaced(client.placed[before:], exchange.SideSell)
	if requoted == nil || math.Abs(requoted.Size-6.4) > 1e-9 {
		t.Fatalf("the ask must be requoted at what the account can carry, 6.4 contracts: %+v", requoted)
	}
}

// Any venue cancel (reduce_only_no_position, an operator's) is not a fill.
func TestAnyVenueCancelIsNotAFill(t *testing.T) {
	bot, client, registry := engineFutureBot(t)
	ctx := context.Background()
	if err := bot.RunCycle(ctx); err != nil {
		t.Fatal(err)
	}
	bid, ask := *lastPlaced(client.placed, exchange.SideBuy), *lastPlaced(client.placed, exchange.SideSell)
	client.openOrders = []exchange.Order{restingFrom(bid), restingFrom(ask)}
	if err := bot.RunCycle(ctx); err != nil {
		t.Fatal(err)
	}
	client.openOrders = []exchange.Order{restingFrom(bid)}
	client.reasons[ask.OrderID] = "reduce_only_no_position"
	if err := bot.RunCycle(ctx); err != nil {
		t.Fatal(err)
	}
	if fills, _, _ := registry.SnapshotCounters(); fills[string(exchange.SideSell)] != 0 {
		t.Fatalf("a venue cancel is not a fill: %v", fills)
	}
}

// At the cutoff (an hour before the fixing window here) the bot takes its quotes off and places nothing.
func TestTheBotTakesItsQuotesOffAtTheCutoff(t *testing.T) {
	bot, client, _ := engineFutureBot(t)
	ctx := context.Background()
	if err := bot.RunCycle(ctx); err != nil {
		t.Fatal(err)
	}
	bid, ask := *lastPlaced(client.placed, exchange.SideBuy), *lastPlaced(client.placed, exchange.SideSell)
	client.openOrders = []exchange.Order{restingFrom(bid), restingFrom(ask)}
	now := time.Now().UTC()
	client.future.FixingStart, client.future.LastTradeTime = now.Add(59*time.Minute), now.Add(2*time.Hour)
	before := len(client.placed)
	if err := bot.RunCycle(ctx); err != nil {
		t.Fatal(err)
	}
	if len(client.placed) != before {
		t.Fatalf("placed %d orders after the cutoff", len(client.placed)-before)
	}
	gone := map[string]bool{}
	for _, id := range client.cancelled {
		gone[id] = true
	}
	if !gone[bid.OrderID] || !gone[ask.OrderID] {
		t.Fatalf("both resting quotes must be cancelled at the cutoff: %v", client.cancelled)
	}
}

// A cycle that cannot read the future halts the bot and cancels its quotes: without the band, the margin
// and the cutoff, a resting quote can fill at a price the bot no longer stands behind.
func TestAnUnreadableFutureHaltsTheBotAndCancelsItsQuotes(t *testing.T) {
	bot, client, _ := engineFutureBot(t)
	ctx := context.Background()
	if err := bot.RunCycle(ctx); err != nil {
		t.Fatal(err)
	}
	bid, ask := *lastPlaced(client.placed, exchange.SideBuy), *lastPlaced(client.placed, exchange.SideSell)
	client.openOrders = []exchange.Order{restingFrom(bid), restingFrom(ask)}
	client.stateErr = errors.New("index unreadable (a stale feed reverts BLF_DataTooOld)")
	before := len(client.cancelled)
	_ = bot.RunCycle(ctx)
	if !bot.currentHalted || len(client.cancelled) == before {
		t.Fatalf("halted %v, cancelled %d", bot.currentHalted, len(client.cancelled)-before)
	}
}

// The anchor is the future's own index, carried to expiry: the number its fill band is set around. Any
// other anchor is refused at startup.
func TestTheEngineFutureIsPricedOffItsOwnIndex(t *testing.T) {
	ctx := context.Background()
	_, base, _ := engineFutureBot(t)
	spec := base.spec
	spec.ExpiryTimestamp = time.Now().UTC().Add(365 * 24 * time.Hour).Unix()
	bot, client, _ := engineFutureBotWithSpec(t, spec)
	if err := bot.RunCycle(ctx); err != nil {
		t.Fatal(err)
	}
	// a year at 20%: 1,400 x e^0.2 = 1,709.98; outside the 1,540 band, so the bid clamps to its inner
	// edge and the ask, above the band, is dropped
	if want := 1400 * math.Exp(0.2); math.Abs(bot.snapshot.AnchorPrice-want) > 0.01 {
		t.Fatalf("anchor %v, want the index carried a year at 20%%: %v", bot.snapshot.AnchorPrice, want)
	}
	if ask := lastPlaced(client.placed, exchange.SideSell); ask != nil {
		t.Fatalf("an ask above the band must not be placed: %+v", ask)
	}

	refused, _, _ := engineFutureBot(t)
	refused.cfg.AnchorSourceType = "oracle_carry"
	if err := refused.Initialize(ctx); err == nil {
		t.Fatal("the MarginEngine future priced off another oracle must be refused")
	}
}

func engineFutureBotWithSpec(t *testing.T, spec exchange.MarketSpec) (*Bot, *engineFutureClient, *metrics.Registry) {
	t.Helper()
	_, client, _ := engineFutureBot(t)
	client.spec = spec
	cfg := config.Config{
		MarketSymbol: "USDC-cNGN-31DEC26", StateFile: filepath.Join(t.TempDir(), "state.json"),
		OrderSize: 10, HalfSpreadBPS: 10, MaxLongInventory: 100, MaxShortInventory: -100,
		QuoteRefreshInterval: 0, CancelStaleOrderThreshold: 20, AdoptSizeTolerance: 0.01, MaxCancelsPerMinute: 1000,
		FutureIMUsage: 0.8, FutureBandBufferBPS: 10, FutureCutoffBeforeFixSeconds: 3600,
		AnchorSourceType: "future_index_carry", CarryRateAPR: 0.2,
	}
	registry := metrics.New()
	bot := NewBot(cfg, client, spec, registry, slog.New(slog.NewTextHandler(io.Discard, nil)), state.NewStore(cfg.StateFile))
	return bot, client, registry
}
