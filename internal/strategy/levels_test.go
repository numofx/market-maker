package strategy

import (
	"testing"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/state"
)

func spotSpec() exchange.MarketSpec {
	return exchange.MarketSpec{
		Symbol:     "USDCcNGN-SPOT",
		BaseAsset:  "USDC",
		QuoteAsset: "cNGN",
		TickSize:   0.000000000000000001,
		SizeStep:   0.000001,
		MinSize:    0.000001,
	}
}

// A funded snapshot with a local mid so the reference resolves without an anchor.
func spotSnapshot(bid, ask, usdc, cngn float64) state.Snapshot {
	return state.Snapshot{
		Market:               "USDCcNGN-SPOT",
		BestBid:              bid,
		BestAsk:              ask,
		LocalReferencePrice:  (bid + ask) / 2,
		LocalReferenceSource: "book",
		Positions: map[string]state.AssetPosition{
			"USDC": {Total: usdc, Available: usdc},
			"cNGN": {Total: cngn, Available: cngn},
		},
		InventoryByAsset: map[string]float64{"USDC": 0, "cNGN": cngn},
	}
}

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

func TestBuildQuotes_SingleLevelUnchanged(t *testing.T) {
	cfg := baseCfg()
	snap := spotSnapshot(1370, 1374, 1000, 1_000_000)
	res, err := BuildQuotes(cfg, spotSpec(), snap)
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

func TestBuildQuotes_LadderStepsOutwardWithBoundedSize(t *testing.T) {
	cfg := baseCfg()
	cfg.QuoteLevels = 4
	cfg.LevelSpreadStepBPS = 20
	cfg.OrderSize = 3
	cfg.MaxNotionalPerSide = 0 // capacity-only limit
	// 1000 USDC and 1e6 cNGN — plenty for a few 3-USDC levels per side.
	snap := spotSnapshot(1370, 1374, 1000, 1_000_000)
	res, err := BuildQuotes(cfg, spotSpec(), snap)
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

func TestBuildQuotes_LadderRespectsNotionalCap(t *testing.T) {
	cfg := baseCfg()
	cfg.QuoteLevels = 5
	cfg.LevelSpreadStepBPS = 15
	cfg.OrderSize = 4
	cfg.MaxNotionalPerSide = 20 // ~ cap total bid notional; at ~1370, max ~0.0146 USDC/... capacity-bound
	snap := spotSnapshot(1370, 1374, 1000, 1_000_000)
	res, err := BuildQuotes(cfg, spotSpec(), snap)
	if err != nil {
		t.Fatalf("BuildQuotes: %v", err)
	}
	// Sum of bid sizes must not exceed the per-side notional budget / price.
	var totalBid float64
	for _, q := range res.Bids {
		totalBid += q.Size
	}
	maxBid := cfg.MaxNotionalPerSide / res.Bids[0].Price
	if totalBid > maxBid+1e-9 {
		t.Fatalf("total bid size %v exceeds notional budget %v", totalBid, maxBid)
	}
}

func TestBuildQuotes_LadderRespectsInventoryLimit(t *testing.T) {
	cfg := baseCfg()
	cfg.QuoteLevels = 6
	cfg.LevelSpreadStepBPS = 10
	cfg.OrderSize = 5
	cfg.MaxNetInventory = 12 // caps cumulative long build-up
	snap := spotSnapshot(1370, 1374, 10_000, 1_000_000)
	res, err := BuildQuotes(cfg, spotSpec(), snap)
	if err != nil {
		t.Fatalf("BuildQuotes: %v", err)
	}
	var totalBid float64
	for _, q := range res.Bids {
		totalBid += q.Size
	}
	if totalBid > cfg.MaxNetInventory+1e-9 {
		t.Fatalf("cumulative bid size %v exceeds net inventory cap %v", totalBid, cfg.MaxNetInventory)
	}
}
