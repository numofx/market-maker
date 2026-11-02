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

type optionClient struct {
	integrationClient
	option   exchange.OptionState
	stateErr error
}

func (c *optionClient) OptionState(context.Context, exchange.MarketSpec) (exchange.OptionState, error) {
	return c.option, c.stateErr
}

// The ATM put of a series 25 days out, on an account with $2,000 of IM surplus and no position.
func optionBot(t *testing.T) (*Bot, *optionClient) {
	t.Helper()
	now := time.Now().UTC()
	expiry := now.Add(25 * 24 * time.Hour).Unix()
	client := &optionClient{
		integrationClient: integrationClient{
			spec: exchange.MarketSpec{Symbol: "cNGN-30NOV26-0.00072-P", Kind: exchange.MarketKindOption, BaseAsset: "cNGN", QuoteAsset: "USDC",
				TickSize: 1e-18, SizeStep: 1000, MinSize: 1000, TakerFeeBps: 15, AmountScale: "1000000000000000000000",
				MarginManager: "0x0705219a1594d5fc5dc2573da4082caf3e1b0950",
				Option:        &exchange.OptionTerms{Strike: 0.00072, Expiry: expiry, LastTradeTime: expiry - 3600, UnitBase: 1000}},
			balances: []exchange.Balance{{Asset: "cNGN"}, {Asset: "USDC", Total: 2000, Available: 2000}},
		},
		option: exchange.OptionState{OptionInputs: exchange.OptionInputs{BlockTime: now.Unix(), Forward: 0.000721, Vol: 0.40, VolConfidence: 1},
			IMSurplus: 2000, WriteIMPerBase: 0.000242},
	}
	cfg := config.Config{
		MarketSymbol: client.spec.Symbol, StateFile: filepath.Join(t.TempDir(), "state.json"),
		OrderSize: 1, MaxLongInventory: 300_000, MaxShortInventory: -300_000,
		QuoteRefreshInterval: 0, CancelStaleOrderThreshold: 20, AdoptSizeTolerance: 0.01, MaxCancelsPerMinute: 1000,
		Option: config.OptionConfig{HalfSpreadVol: 0.03, MinHalfSpread: 0.0000005, OrderSize: 100_000, MaxPosition: 200_000, IMUsage: 0.5,
			CutoffSeconds: 1800, DegradedSpreadMultiplier: 2, DeltaSkewBPS: 50, DeltaFullAt: 500_000},
	}
	bot := NewBot(cfg, client, client.spec, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)), state.NewStore(cfg.StateFile))
	return bot, client
}

// Through RunCycle: both sides are placed, each with its side, at Black-76 prices either side of fair.
func TestTheOptionBotPlacesBothSides(t *testing.T) {
	bot, client := optionBot(t)
	if err := bot.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	bid, ask := lastPlaced(client.placed, exchange.SideBuy), lastPlaced(client.placed, exchange.SideSell)
	if bid == nil || ask == nil {
		t.Fatalf("placed %+v", client.placed)
	}
	if !(bid.Price < bot.snapshot.ReferencePrice && bot.snapshot.ReferencePrice < ask.Price) || bid.Size != 100_000 || ask.Size != 100_000 {
		t.Fatalf("bid %+v ask %+v around %v", bid, ask, bot.snapshot.ReferencePrice)
	}
}

// An option the bot cannot read halts it and cancels its quotes (on the fork: the forward reverts once the spot
// index is past its heartbeat).
func TestAnUnreadableOptionHaltsTheBotAndCancelsItsQuotes(t *testing.T) {
	bot, client := optionBot(t)
	ctx := context.Background()
	if err := bot.RunCycle(ctx); err != nil {
		t.Fatal(err)
	}
	client.openOrders = []exchange.Order{restingFrom(*lastPlaced(client.placed, exchange.SideBuy)), restingFrom(*lastPlaced(client.placed, exchange.SideSell))}
	client.stateErr = errors.New("getForwardPrice: execution reverted")
	before := len(client.cancelled)
	_ = bot.RunCycle(ctx)
	if !bot.currentHalted || len(client.cancelled)-before != 2 {
		t.Fatalf("halted %v, cancelled %d", bot.currentHalted, len(client.cancelled)-before)
	}
}

// A vol feed with no live surface is not an error: the strike is pulled, its resting quotes cancelled.
func TestNoVolSurfacePullsTheStrike(t *testing.T) {
	bot, client := optionBot(t)
	ctx := context.Background()
	if err := bot.RunCycle(ctx); err != nil {
		t.Fatal(err)
	}
	client.openOrders = []exchange.Order{restingFrom(*lastPlaced(client.placed, exchange.SideBuy)), restingFrom(*lastPlaced(client.placed, exchange.SideSell))}
	client.option = exchange.OptionState{VolUnavailable: true}
	before, placed := len(client.cancelled), len(client.placed)
	_ = bot.RunCycle(ctx)
	if len(client.cancelled)-before != 2 || len(client.placed) != placed {
		t.Fatalf("cancelled %d, placed %d more", len(client.cancelled)-before, len(client.placed)-placed)
	}
}
