package strategy

import (
	"math"
	"testing"
	"time"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/pricing"
	"github.com/numofx/market-maker/internal/state"
)

const (
	optExpiry    = 1796047200
	optLastTrade = 1796043600
	optBlockTime = optExpiry - 25*86400
)

func optCfg() config.Config {
	return config.Config{SubaccountID: "900", Option: config.OptionConfig{HalfSpreadVol: 0.03, MinHalfSpread: 0.0000005, OrderSize: 100_000,
		MaxPosition: 200_000, IMUsage: 0.5, CutoffSeconds: 1800, DegradedSpreadMultiplier: 2, DeltaSkewBPS: 50, DeltaFullAt: 500_000}}
}

func optSpec(strike float64, call bool) exchange.MarketSpec {
	return exchange.MarketSpec{Symbol: "cNGN-30NOV26-X", Kind: exchange.MarketKindOption, BaseAsset: "cNGN", QuoteAsset: "USDC",
		TickSize: 1e-18, SizeStep: 1000, MinSize: 1000,
		Option: &exchange.OptionTerms{Strike: strike, Call: call, Expiry: optExpiry, LastTradeTime: optLastTrade, UnitBase: 1000}}
}

func optSnap(inventory float64) state.Snapshot {
	return state.Snapshot{InventoryByAsset: map[string]float64{"cNGN": inventory}, Option: &state.OptionSnapshot{OptionState: exchange.OptionState{
		OptionInputs: exchange.OptionInputs{BlockTime: optBlockTime, Forward: 0.000721, Vol: 0.40, VolConfidence: 1},
		IMSurplus:    10_000, WriteIMPerBase: 0.000244}}}
}

var optNow = time.Unix(optBlockTime, 0)

func buildOpt(t *testing.T, cfg config.Config, spec exchange.MarketSpec, snap state.Snapshot) Result {
	t.Helper()
	r, err := BuildQuotesWithOverrides(cfg, spec, snap, NoOverrides())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func black(strike, vol, fwd float64, call bool) float64 {
	return pricing.Option{Forward: fwd, Strike: strike, Vol: vol, Seconds: 25 * 86400, Call: call}.Price()
}

// The ATM put quotes 3 vol points either side of the surface, around the engine's own mark.
func TestOptionQuotesSitThreeVolPointsOffTheSurface(t *testing.T) {
	r := buildOptionQuotes(optCfg(), optSpec(0.00072, false), optSnap(0), NoOverrides(), optNow)
	if r.Bid == nil || r.Ask == nil {
		t.Fatalf("both sides quote: %+v %+v", r.BidSuppression, r.AskSuppression)
	}
	fair := black(0.00072, 0.40, 0.000721, false)
	if math.Abs(r.ReferencePrice-fair) > 1e-15 || r.ReferenceSource != "black76" {
		t.Fatalf("reference %v (%s), want the engine's mark %v", r.ReferencePrice, r.ReferenceSource, fair)
	}
	if want := black(0.00072, 0.37, 0.000721, false); math.Abs(r.Bid.Price-want) > 1e-17 {
		t.Fatalf("bid %v, want the 37-vol premium %v", r.Bid.Price, want)
	}
	if want := black(0.00072, 0.43, 0.000721, false); math.Abs(r.Ask.Price-want) > 1e-17 {
		t.Fatalf("ask %v, want the 43-vol premium %v", r.Ask.Price, want)
	}
	// The syncer places what the quote says: a side-less quote is refused at the client ("invalid side").
	if r.Bid.Side != exchange.SideBuy || r.Ask.Side != exchange.SideSell || r.Bids[0].Side != exchange.SideBuy || r.Asks[0].Side != exchange.SideSell {
		t.Fatalf("sides: bid %q ask %q", r.Bid.Side, r.Ask.Side)
	}
	if r.Bid.Size != 100_000 || r.Ask.Size != 100_000 {
		t.Fatalf("sizes %v / %v", r.Bid.Size, r.Ask.Size)
	}
}

// A far strike's vega is tiny: the premium floor keeps the quote off fair.
func TestDeepOTMQuotesKeepTheMinimumPremiumSpread(t *testing.T) {
	r := buildOptionQuotes(optCfg(), optSpec(0.00095, true), optSnap(0), NoOverrides(), optNow)
	if r.Ask == nil || r.Ask.Price < r.ReferencePrice+0.0000005-1e-18 {
		t.Fatalf("ask %+v within the floor of fair %v", r.Ask, r.ReferencePrice)
	}
	if r.Bid != nil {
		t.Fatalf("a bid under the floor is below zero and pulled, got %+v", r.Bid)
	}
	if r.BidSuppression == nil || r.BidSuppression.Reason != reasonOptionNoPremium {
		t.Fatalf("bid suppression %+v", r.BidSuppression)
	}
}

func TestOptionSizesRespectPositionAndMargin(t *testing.T) {
	// Short 150k: 50k more may be written, 350k bought (rounded to whole 1,000-cNGN units, capped at the order size).
	r := buildOptionQuotes(optCfg(), optSpec(0.00072, false), optSnap(-150_000), NoOverrides(), optNow)
	if r.Ask == nil || r.Ask.Size != 50_000 || r.Bid == nil || r.Bid.Size != 100_000 {
		t.Fatalf("short 150k: bid %+v ask %+v", r.Bid, r.Ask)
	}
	// At the limit the written side is pulled.
	r = buildOptionQuotes(optCfg(), optSpec(0.00072, false), optSnap(-200_000), NoOverrides(), optNow)
	if r.Ask != nil || r.AskSuppression.Reason != reasonOptionMaxPosition {
		t.Fatalf("at max short: ask %+v %+v", r.Ask, r.AskSuppression)
	}
	// $10 of surplus at 50% usage writes 5 / 0.000244 = 20,491 cNGN: 20 units.
	snap := optSnap(0)
	snap.Option.IMSurplus = 10
	r = buildOptionQuotes(optCfg(), optSpec(0.00072, false), snap, NoOverrides(), optNow)
	if r.Ask == nil || r.Ask.Size != 20_000 {
		t.Fatalf("margin-bound ask %+v %+v", r.Ask, r.AskSuppression)
	}
	// No surplus: nothing new is written, but a long can still be sold down.
	snap = optSnap(30_000)
	snap.Option.IMSurplus = -5
	r = buildOptionQuotes(optCfg(), optSpec(0.00072, false), snap, NoOverrides(), optNow)
	if r.Ask == nil || r.Ask.Size != 30_000 || r.Bid != nil || r.BidSuppression.Reason != reasonOptionCapacity {
		t.Fatalf("no surplus, long 30k: bid %+v %+v ask %+v", r.Bid, r.BidSuppression, r.Ask)
	}
}

func TestOptionPullsAndWidens(t *testing.T) {
	cases := map[string]func(*state.Snapshot, *config.Config){
		reasonVolUnavailable:     func(s *state.Snapshot, _ *config.Config) { s.Option.VolUnavailable = true },
		reasonOptionStateMissing: func(s *state.Snapshot, _ *config.Config) { s.Option = nil },
		reasonOptionCutoff:       func(s *state.Snapshot, _ *config.Config) { s.Option.BlockTime = optLastTrade - 1800 },
		reasonVolDegraded: func(s *state.Snapshot, c *config.Config) {
			s.Option.VolConfidence = 0.5
			c.Option.DegradedSpreadMultiplier = 0
		},
	}
	for reason, mutate := range cases {
		snap, cfg := optSnap(0), optCfg()
		mutate(&snap, &cfg)
		r := buildOptionQuotes(cfg, optSpec(0.00072, true), snap, NoOverrides(), optNow)
		if r.Bid != nil || r.Ask != nil || r.BidSuppression == nil || r.BidSuppression.Reason != reason || r.AskSuppression.Reason != reason {
			t.Fatalf("%s: bid %+v ask %+v (%+v)", reason, r.Bid, r.Ask, r.BidSuppression)
		}
	}
	// The wall clock past the cutoff pulls too, whatever block was read.
	if r := buildOptionQuotes(optCfg(), optSpec(0.00072, true), optSnap(0), NoOverrides(), time.Unix(optLastTrade-60, 0)); r.Bid != nil {
		t.Fatal("quoted past the cutoff by the clock")
	}
	snap := optSnap(0)
	snap.Option.VolConfidence = 0.5
	r := buildOptionQuotes(optCfg(), optSpec(0.00072, true), snap, NoOverrides(), optNow)
	if want := black(0.00072, 0.46, 0.000721, true); r.Ask == nil || math.Abs(r.Ask.Price-want) > 1e-17 {
		t.Fatalf("degraded: ask %+v, want 6 vol points over (%v)", r.Ask, want)
	}
}

// Long delta across the book lowers the calls and raises the puts, so the next fills reduce it.
func TestPortfolioDeltaSkewsCallsDownAndPutsUp(t *testing.T) {
	flat, long := optSnap(0), optSnap(0)
	long.Option.PortfolioDelta = 500_000
	callFlat := buildOptionQuotes(optCfg(), optSpec(0.00072, true), flat, NoOverrides(), optNow)
	callLong := buildOptionQuotes(optCfg(), optSpec(0.00072, true), long, NoOverrides(), optNow)
	putFlat := buildOptionQuotes(optCfg(), optSpec(0.00072, false), flat, NoOverrides(), optNow)
	putLong := buildOptionQuotes(optCfg(), optSpec(0.00072, false), long, NoOverrides(), optNow)
	if !(callLong.Bid.Price < callFlat.Bid.Price && callLong.Ask.Price < callFlat.Ask.Price) {
		t.Fatalf("long delta: call %v/%v vs flat %v/%v", callLong.Bid.Price, callLong.Ask.Price, callFlat.Bid.Price, callFlat.Ask.Price)
	}
	if !(putLong.Bid.Price > putFlat.Bid.Price && putLong.Ask.Price > putFlat.Ask.Price) {
		t.Fatalf("long delta: put %v/%v vs flat %v/%v", putLong.Bid.Price, putLong.Ask.Price, putFlat.Bid.Price, putFlat.Ask.Price)
	}
	if callLong.ReferencePrice != callFlat.ReferencePrice || callLong.SkewBPS != 50 {
		t.Fatalf("the reference stays the engine's mark; skew %v", callLong.SkewBPS)
	}
}

// The dispatch: BuildQuotesWithOverrides prices an option market through the option strategy, not the bps ladder.
func TestBuildQuotesRoutesOptions(t *testing.T) {
	r := buildOpt(t, optCfg(), optSpec(0.00072, false), optSnap(0))
	if r.ReferenceSource != "black76" {
		t.Fatalf("an option was priced by %q", r.ReferenceSource)
	}
}
