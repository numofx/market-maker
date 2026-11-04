package execution

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"testing"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/metrics"
	"github.com/numofx/market-maker/internal/state"
	"github.com/numofx/market-maker/internal/strategy"
)

// feeClient serves a taker fee the test controls and records cancellations, so the assertions are
// about what the bot DOES with a schedule change rather than what it logged.
type feeClient struct {
	mockClient
	takerFeeBps  int
	getMarketErr error
}

func (f *feeClient) GetMarket(context.Context, string) (exchange.MarketSpec, error) {
	if f.getMarketErr != nil {
		return exchange.MarketSpec{}, f.getMarketErr
	}
	return exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", TakerFeeBps: f.takerFeeBps}, nil
}

// restingLadder is what a quiet market looks like: quotes on the book that reconcileSide would
// happily leave alone, because their price has not drifted.
func restingLadder() []exchange.Order {
	return []exchange.Order{
		{ID: "bid-1", Side: exchange.SideBuy, Price: 1324.7, Size: 1.2},
		{ID: "ask-1", Side: exchange.SideSell, Price: 1322.1, Size: 1.2},
	}
}

func feeBot(t *testing.T, client *feeClient, startingFeeBps int) *Bot {
	t.Helper()
	spec := exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", TakerFeeBps: startingFeeBps}
	cfg := config.Config{MarketSymbol: "USDCcNGN-SPOT"}
	return NewBot(cfg, client, spec, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
}

// The case that actually costs money. reconcileSide keeps an order whose price has not drifted, so
// on a quiet market a quote signed under the old, lower bound rests untouched and reverts
// TM_FeeTooHigh on every cross. A rise has to force the ladder off the book.
func TestAFeeRiseCancelsTheRestingLadder(t *testing.T) {
	client := &feeClient{takerFeeBps: 40}
	client.openOrders = restingLadder()
	bot := feeBot(t, client, 25)

	if err := bot.syncFeeSchedule(context.Background()); err != nil {
		t.Fatalf("syncFeeSchedule: %v", err)
	}
	if len(client.cancelled) != 2 {
		t.Fatalf("cancelled %v, want both resting orders off the book after a fee rise", client.cancelled)
	}
	if bot.spec.TakerFeeBps != 40 {
		t.Fatalf("bot still holds %d bps; the next quote would sign the old bound", bot.spec.TakerFeeBps)
	}
}

// A cut leaves every resting bound comfortably above the new charge. Churning the book for it
// would spend cancel budget to no purpose.
func TestAFeeCutKeepsTheLadderInPlace(t *testing.T) {
	client := &feeClient{takerFeeBps: 5}
	client.openOrders = restingLadder()
	bot := feeBot(t, client, 25)

	if err := bot.syncFeeSchedule(context.Background()); err != nil {
		t.Fatalf("syncFeeSchedule: %v", err)
	}
	if len(client.cancelled) != 0 {
		t.Fatalf("cancelled %v, want nothing cancelled on a fee cut", client.cancelled)
	}
	if bot.spec.TakerFeeBps != 5 {
		t.Fatalf("bot holds %d bps, want the new 5", bot.spec.TakerFeeBps)
	}
}

// The steady state is every cycle. It must not cancel anything.
func TestAnUnchangedScheduleTouchesNothing(t *testing.T) {
	client := &feeClient{takerFeeBps: 25}
	client.openOrders = restingLadder()
	bot := feeBot(t, client, 25)

	for i := 0; i < 5; i++ {
		if err := bot.syncFeeSchedule(context.Background()); err != nil {
			t.Fatalf("syncFeeSchedule: %v", err)
		}
	}
	if len(client.cancelled) != 0 {
		t.Fatalf("cancelled %v across five unchanged cycles, want nothing", client.cancelled)
	}
}

// GetMarket already falls back to the last good schedule internally, so an error out of it means
// there never was one. Quoting against a schedule nobody has seen is not something to guess at.
func TestNoScheduleAtAllStopsTheCycle(t *testing.T) {
	client := &feeClient{getMarketErr: context.DeadlineExceeded}
	bot := feeBot(t, client, 0)

	if err := bot.syncFeeSchedule(context.Background()); err == nil {
		t.Fatal("want an error when the schedule could never be loaded")
	}
	if len(client.cancelled) != 0 {
		t.Fatalf("cancelled %v, want nothing when the schedule is simply unknown", client.cancelled)
	}
}

// scheduleBoundClient derives the required bound from the spec it is handed, as the real client does, so the
// test can see which schedule the syncer judged an order against.
type scheduleBoundClient struct{ feeClient }

func (c *scheduleBoundClient) RequiredWorstFee(spec exchange.MarketSpec, _ float64) (string, error) {
	return strconv.Itoa(spec.TakerFeeBps * 1000), nil
}

// Orders signed after a schedule change must not read as stale. The syncer used to keep the startup schedule
// after a cut, so spot and both futures makers replaced their whole ladder every cycle for four days.
func TestAScheduleChangeReachesTheOrdersTheSyncerJudges(t *testing.T) {
	for _, tc := range []struct {
		name               string
		startBps, venueBps int
	}{
		{"cut", 25, 15},
		{"rise", 25, 40},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &scheduleBoundClient{feeClient{takerFeeBps: tc.venueBps}}
			bot := NewBot(config.Config{MarketSymbol: "USDCcNGN-SPOT", CancelStaleOrderThreshold: 20}, client,
				exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", TakerFeeBps: tc.startBps}, metrics.New(),
				slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
			if err := bot.syncFeeSchedule(context.Background()); err != nil {
				t.Fatalf("syncFeeSchedule: %v", err)
			}
			client.cancelled = nil

			// Signed by the client under the schedule the venue serves now.
			signed := restingOrder("1", exchange.SideBuy, false, strconv.Itoa(tc.venueBps*1000))
			quotes := strategy.Result{Bid: &strategy.Quote{Side: exchange.SideBuy, Price: signed.Price, Size: signed.Size}}
			ids := map[exchange.Side][]Identity{exchange.SideBuy: {{OrderID: "next", Nonce: "2"}}}
			if _, err := bot.syncer.Sync(context.Background(), state.Snapshot{
				Market: "USDCcNGN-SPOT", OpenOrders: []exchange.Order{signed},
			}, quotes, ids); err != nil {
				t.Fatalf("Sync: %v", err)
			}
			if len(client.cancelled) != 0 {
				t.Fatalf("cancelled %v: an order signed under the current schedule was replaced", client.cancelled)
			}
		})
	}
}
