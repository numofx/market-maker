package strategy

import (
	"math"
	"testing"
	"time"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/state"
)

func engineFutureSpec() exchange.MarketSpec {
	return exchange.MarketSpec{Symbol: "USDC-cNGN-31DEC26", Kind: exchange.MarketKindFuture, BaseAsset: "USDC", QuoteAsset: "cNGN",
		AssetAddress: "0x00000000000000000000000000000000000000f0", SubID: "1798675200", ExpiryTimestamp: 1798675200,
		TickSize: 0.000001, SizeStep: 0.001, MinSize: 0.001, TakerFeeBps: 25,
		MarginManager: "0x00000000000000000000000000000000000000e9", ContractSizeUSDC: 1000}
}

func engineFutureConfig() config.Config {
	return config.Config{MarketSymbol: "USDC-cNGN-31DEC26", OrderSize: 5, HalfSpreadBPS: 20,
		MaxLongInventory: 100, MaxShortInventory: -100, QuoteLevels: 1,
		FutureIMUsage: 0.8, FutureBandBufferBPS: 10, FutureCutoffBeforeFixSeconds: 3600}
}

// 10,000 USD of IM surplus, 250 a contract (25% of 1,000 USDC), the index at 1,400 with a 10% band,
// fixing a week out.
func engineFutureState(now time.Time) *exchange.FutureState {
	return &exchange.FutureState{IMSurplus: 10_000, PerContractIM: 250, Index: 1400, BandWidth: 0.1, BandLow: 1260, BandHigh: 1540,
		LastTradeTime: now.Add(7*24*time.Hour + time.Hour), FixingStart: now.Add(7 * 24 * time.Hour)}
}

func engineFutureSnapshot(future *exchange.FutureState, position float64, ref float64) state.Snapshot {
	return state.Snapshot{
		Market: "USDC-cNGN-31DEC26", AnchorPrice: ref, AnchorSource: "oracle_carry", ReferencePrice: ref,
		InventoryByAsset: map[string]float64{"USDC": position},
		Positions: map[string]state.AssetPosition{
			"USDC": {Total: position},
			// the account's cash: irrelevant to the engine future's sizing, so deliberately tiny
			"cNGN": {Total: 1, Available: 1},
		},
		Future: future,
	}
}

func build(t *testing.T, cfg config.Config, snapshot state.Snapshot) Result {
	t.Helper()
	got, err := BuildQuotes(cfg, engineFutureSpec(), snapshot)
	if err != nil {
		t.Fatalf("BuildQuotes: %v", err)
	}
	return got
}

// Capacity is the IM headroom in contracts, not cash: 10,000 x 0.8 / 250 = 32 contracts a side, with 1
// USDC of cash. Closing contracts need no IM.
func TestTheEngineFutureIsSizedByIMHeadroomNotCash(t *testing.T) {
	now := time.Now().UTC()
	cfg := engineFutureConfig()
	cfg.OrderSize = 1000
	got := build(t, cfg, engineFutureSnapshot(engineFutureState(now), 0, 1400))
	if got.Bid == nil || got.Ask == nil || math.Abs(got.Bid.Size-32) > 1e-9 || math.Abs(got.Ask.Size-32) > 1e-9 {
		t.Fatalf("each side 32 contracts: bid %+v ask %+v (%+v / %+v)", got.Bid, got.Ask, got.BidSuppression, got.AskSuppression)
	}
	// long 10: the ask closes 10 with no IM, then opens 32
	got = build(t, cfg, engineFutureSnapshot(engineFutureState(now), 10, 1400))
	if math.Abs(got.Ask.Size-42) > 1e-9 || math.Abs(got.Bid.Size-32) > 1e-9 {
		t.Fatalf("long 10: ask 42, bid 32: bid %+v ask %+v", got.Bid, got.Ask)
	}
	// no IM surplus: nothing opens
	short := engineFutureState(now)
	short.IMSurplus = -5
	got = build(t, cfg, engineFutureSnapshot(short, 0, 1400))
	if got.Bid != nil || got.Ask != nil {
		t.Fatalf("no IM headroom quotes nothing: bid %+v ask %+v", got.Bid, got.Ask)
	}
}

// MM_MAX_NOTIONAL_PER_SIDE is USDC, and a contract is 1,000 USDC whatever its price: 5,000 USDC caps a
// side at 5 contracts. The old model divided by the price (5,000 / 1,400 = 3.571 contracts).
func TestTheEngineFutureNotionalIsContractsTimesContractSize(t *testing.T) {
	cfg := engineFutureConfig()
	cfg.OrderSize = 1000
	cfg.MaxNotionalPerSide = 5000
	got := build(t, cfg, engineFutureSnapshot(engineFutureState(time.Now().UTC()), 0, 1400))
	if got.Bid == nil || math.Abs(got.Bid.Size-5) > 1e-9 || math.Abs(got.Ask.Size-5) > 1e-9 {
		t.Fatalf("5,000 USDC is 5 contracts a side: bid %+v ask %+v", got.Bid, got.Ask)
	}
}

// From the cutoff (an hour before the fixing window or last trade, whichever is first) nothing is quoted,
// and the syncer takes resting orders off.
func TestTheEngineFutureStopsQuotingAtItsCutoff(t *testing.T) {
	cfg := engineFutureConfig()
	now := time.Now().UTC()
	for name, future := range map[string]*exchange.FutureState{
		"an hour before fixing":    {IMSurplus: 10_000, PerContractIM: 250, BandLow: 1260, BandHigh: 1540, FixingStart: now.Add(time.Hour), LastTradeTime: now.Add(2 * time.Hour)},
		"last trade before fixing": {IMSurplus: 10_000, PerContractIM: 250, BandLow: 1260, BandHigh: 1540, FixingStart: now.Add(5 * time.Hour), LastTradeTime: now.Add(30 * time.Minute)},
		"inside the fixing window": {IMSurplus: 10_000, PerContractIM: 250, BandLow: 1260, BandHigh: 1540, FixingStart: now.Add(-time.Minute), LastTradeTime: now.Add(time.Hour)},
		"settled":                  {IMSurplus: 10_000, PerContractIM: 250, BandLow: 1260, BandHigh: 1540, FixingStart: now.Add(48 * time.Hour), LastTradeTime: now.Add(48 * time.Hour), Settled: true},
	} {
		got := build(t, cfg, engineFutureSnapshot(future, 0, 1400))
		if got.Bid != nil || got.Ask != nil || len(got.Bids)+len(got.Asks) != 0 {
			t.Errorf("%s: still quoting: %+v %+v", name, got.Bid, got.Ask)
		}
		if got.BidSuppression == nil || got.AskSuppression == nil {
			t.Errorf("%s: suppression must say why", name)
		}
	}
	// an hour and a minute before fixing: still quoting
	ok := engineFutureState(now)
	ok.FixingStart, ok.LastTradeTime = now.Add(61*time.Minute), now.Add(2*time.Hour)
	if got := build(t, cfg, engineFutureSnapshot(ok, 0, 1400)); got.Bid == nil || got.Ask == nil {
		t.Fatal("before the cutoff the future quotes")
	}
	// missing state is not quoted at all
	if got := build(t, cfg, engineFutureSnapshot(nil, 0, 1400)); got.Bid != nil || got.Ask != nil {
		t.Fatal("no future state, no quotes")
	}
}

// Quotes stay inside the fill band, 10 bps in: a bid above it moves down to it, an ask below it moves up;
// a bid below the band or an ask above it cannot be made safe by moving and is dropped.
func TestTheEngineFutureQuotesStayInsideTheFillBand(t *testing.T) {
	cfg := engineFutureConfig()
	now := time.Now().UTC()
	high := 1540 * (1 - 0.001)
	low := 1260 * (1 + 0.001)

	// reference far above the band (1,600): the bid would be 1,596.8, above the band, so it comes down to
	// the band's inner edge; the ask is above the band and is dropped
	got := build(t, cfg, engineFutureSnapshot(engineFutureState(now), 0, 1600))
	if got.Bid == nil || got.Bid.Price > high+1e-6 || got.Bid.Price < high-1e-5 {
		t.Fatalf("the bid clamps to the band's inner edge %.6f: %+v", high, got.Bid)
	}
	if got.Ask != nil || got.AskSuppression == nil || got.AskSuppression.Reason != reasonOutsideFillBand {
		t.Fatalf("an ask above the band is dropped: %+v %+v", got.Ask, got.AskSuppression)
	}
	// reference below the band (1,200): the ask moves up to the inner low edge, the bid is dropped
	got = build(t, cfg, engineFutureSnapshot(engineFutureState(now), 0, 1200))
	if got.Ask == nil || got.Ask.Price < low-1e-6 || got.Ask.Price > low+1e-5 {
		t.Fatalf("the ask clamps to the band's inner edge %.6f: %+v", low, got.Ask)
	}
	if got.Bid != nil {
		t.Fatalf("a bid below the band is dropped: %+v", got.Bid)
	}
	// inside the band, untouched
	got = build(t, cfg, engineFutureSnapshot(engineFutureState(now), 0, 1400))
	if got.Bid == nil || got.Ask == nil || got.Bid.Price >= 1400 || got.Ask.Price <= 1400 {
		t.Fatalf("inside the band the quote is the strategy's: %+v %+v", got.Bid, got.Ask)
	}
}

func TestClampLevelsMergesLevelsClampedToOnePrice(t *testing.T) {
	levels := []Quote{{Side: exchange.SideBuy, Price: 1600, Size: 1}, {Side: exchange.SideBuy, Price: 1580, Size: 2}, {Side: exchange.SideBuy, Price: 1500, Size: 3}}
	got := clampLevels(levels, exchange.SideBuy, 1260, 1538)
	if len(got) != 2 || got[0].Price != 1538 || got[1].Price != 1500 {
		t.Fatalf("clamped ladder: %+v", got)
	}
}
