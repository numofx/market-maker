package strategy

import (
	"math"
	"testing"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/state"
)

// spotSpec is USDCcNGN-SPOT as the client loads it: cNGN the base, USDC the quote, priced in USDC
// per cNGN, sized in whole cNGN. The tick is coarsened from the venue's 1e-18 so expected prices
// can be written down.
func spotSpec() exchange.MarketSpec {
	return exchange.MarketSpec{
		Symbol:     "USDCcNGN-SPOT",
		BaseAsset:  "cNGN",
		QuoteAsset: "USDC",
		TickSize:   0.000000001,
		SizeStep:   1,
		MinSize:    1,
	}
}

// A funded snapshot with a local mid so the reference resolves without an anchor. cngn is the
// cNGN held (the inventory), usdc the USDC held.
func spotSnapshot(bid, ask, cngn, usdc float64) state.Snapshot {
	return state.Snapshot{
		Market:               "USDCcNGN-SPOT",
		BestBid:              bid,
		BestAsk:              ask,
		LocalReferencePrice:  (bid + ask) / 2,
		LocalReferenceSource: "book",
		Positions: map[string]state.AssetPosition{
			"cNGN": {Total: cngn, Available: cngn},
			"USDC": {Total: usdc, Available: usdc},
		},
		InventoryByAsset: map[string]float64{"cNGN": cngn, "USDC": usdc},
	}
}

// baseCfg: 5 USDC a rung, +/- 1000 USDC of inventory. The operator's units are USDC; the ladder
// is placed in cNGN at the reference.
func baseCfg() config.Config {
	return config.Config{
		HalfSpreadBPS:      10,
		OrderSize:          5,
		MaxLongInventory:   1000,
		MaxShortInventory:  -1000,
		MaxNotionalPerSide: 0,
		QuoteLevels:        1,
		LevelSizeMult:      1,
	}
}

// The production market: ~0.000730 / 0.000734 USDC per cNGN, a bot holding 50 cNGN (36 USDC at
// the mid, inside the inventory bound) and 1,000 USDC.
const (
	liveBid = 0.000730
	liveAsk = 0.000734
	liveMid = (liveBid + liveAsk) / 2
)

func liveSnapshot() state.Snapshot { return spotSnapshot(liveBid, liveAsk, 50_000, 1_000) }

// cngnFor is a USDC size in whole cNGN at a price.
func cngnFor(usdc, price float64) float64 { return math.Floor(usdc / price) }

func TestBuildQuotes_SingleLevelUnchanged(t *testing.T) {
	cfg := baseCfg()
	res, err := BuildQuotes(cfg, spotSpec(), liveSnapshot())
	if err != nil {
		t.Fatalf("BuildQuotes: %v", err)
	}
	if len(res.Bids) != 1 || len(res.Asks) != 1 {
		t.Fatalf("levels = %d bids / %d asks, want 1/1", len(res.Bids), len(res.Asks))
	}
	// The single ladder level must equal the top-of-book Bid/Ask exactly.
	if res.Bid == nil || res.Bids[0] != *res.Bid {
		t.Fatalf("Bids[0] %v != Bid %v", res.Bids[0], res.Bid)
	}
	if res.Ask == nil || res.Asks[0] != *res.Ask {
		t.Fatalf("Asks[0] %v != Ask %v", res.Asks[0], res.Ask)
	}
}

// 5 USDC a rung at ~0.000732 is ~6,830 cNGN, a whole number of them.
func TestBuildQuotes_SizesAreWholeCNGNWorthTheConfiguredUSDC(t *testing.T) {
	res, err := BuildQuotes(baseCfg(), spotSpec(), liveSnapshot())
	if err != nil {
		t.Fatalf("BuildQuotes: %v", err)
	}
	want := cngnFor(5, liveMid)
	for _, q := range []*Quote{res.Bid, res.Ask} {
		if q == nil || q.Size != want || q.Size != math.Floor(q.Size) {
			t.Fatalf("quote %+v, want %v whole cNGN (5 USDC at %v)", q, want, liveMid)
		}
	}
	if res.Bid.Price >= liveMid || res.Ask.Price <= liveMid {
		t.Fatalf("quotes %v / %v do not straddle the mid %v", res.Bid.Price, res.Ask.Price, liveMid)
	}
}

func TestBuildQuotes_LadderStepsOutwardWithBoundedSize(t *testing.T) {
	cfg := baseCfg()
	cfg.QuoteLevels = 4
	cfg.LevelSpreadStepBPS = 20
	cfg.OrderSize = 3
	cfg.MaxNotionalPerSide = 0 // capacity-only limit
	// 50,000 cNGN and 1,000 USDC: plenty for a few 3-USDC levels per side.
	res, err := BuildQuotes(cfg, spotSpec(), liveSnapshot())
	if err != nil {
		t.Fatalf("BuildQuotes: %v", err)
	}
	if len(res.Bids) != 4 || len(res.Asks) != 4 {
		t.Fatalf("levels = %d bids / %d asks, want 4/4", len(res.Bids), len(res.Asks))
	}
	// Bids strictly descend, asks strictly ascend.
	for i := 1; i < len(res.Bids); i++ {
		if res.Bids[i].Price >= res.Bids[i-1].Price {
			t.Fatalf("bid %d price %v not below %v", i, res.Bids[i].Price, res.Bids[i-1].Price)
		}
		if res.Asks[i].Price <= res.Asks[i-1].Price {
			t.Fatalf("ask %d price %v not above %v", i, res.Asks[i].Price, res.Asks[i-1].Price)
		}
	}
	// Every bid stays below every ask (no self-cross).
	if res.Bids[0].Price >= res.Asks[0].Price {
		t.Fatalf("best bid %v crosses best ask %v", res.Bids[0].Price, res.Asks[0].Price)
	}
}

// MM_MAX_NOTIONAL_PER_SIDE is a cNGN amount on the cNGN markets (what production's 450000 has
// always meant): the side's whole ladder, and any one rung, stays under it.
func TestBuildQuotes_LadderRespectsNotionalCapInCNGN(t *testing.T) {
	cfg := baseCfg()
	cfg.QuoteLevels = 5
	cfg.LevelSpreadStepBPS = 15
	cfg.OrderSize = 4 // ~5,460 cNGN a rung
	cfg.MaxNotionalPerSide = 12_000
	res, err := BuildQuotes(cfg, spotSpec(), liveSnapshot())
	if err != nil {
		t.Fatalf("BuildQuotes: %v", err)
	}
	var totalBid float64
	for _, q := range res.Bids {
		totalBid += q.Size
	}
	if totalBid > cfg.MaxNotionalPerSide+1e-9 || len(res.Bids) == 0 {
		t.Fatalf("total bid size %v cNGN exceeds the per-side cap %v", totalBid, cfg.MaxNotionalPerSide)
	}
	if len(res.Bids) >= 5 {
		t.Fatalf("%d bid levels; a 12,000 cNGN budget cannot fund five ~5,460 cNGN rungs", len(res.Bids))
	}
}

// MM_MAX_NET_INVENTORY is USDC: cumulative bids stop where the cNGN held would be worth more.
func TestBuildQuotes_LadderRespectsInventoryLimit(t *testing.T) {
	cfg := baseCfg()
	cfg.QuoteLevels = 6
	cfg.LevelSpreadStepBPS = 10
	cfg.OrderSize = 5
	cfg.MaxNetInventory = 48 // 36.6 USDC held already; room for ~2 rungs of 5
	res, err := BuildQuotes(cfg, spotSpec(), liveSnapshot())
	if err != nil {
		t.Fatalf("BuildQuotes: %v", err)
	}
	var totalBid float64
	for _, q := range res.Bids {
		totalBid += q.Size
	}
	held := 50_000.0
	if (held+totalBid)*liveMid > cfg.MaxNetInventory+1e-9 {
		t.Fatalf("held + bids = %v cNGN = %.2f USDC, over the %v USDC cap", held+totalBid, (held+totalBid)*liveMid, cfg.MaxNetInventory)
	}
	// Two full 5 USDC rungs and a partial third fill the 11.4 USDC of headroom; never all six.
	if len(res.Bids) == 0 || len(res.Bids) >= 6 {
		t.Fatalf("%d bid levels, want the ladder truncated by an 11.4 USDC headroom", len(res.Bids))
	}
	if len(res.Asks) == 0 {
		t.Fatal("the ask side is not bounded by the long limit")
	}
}
