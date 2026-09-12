package strategy

import (
	"testing"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/state"
)

// simulateCycle answers the only question that matters: given the ladder a cycle produced, and
// nothing else changing, does the NEXT cycle want the same thing?
//
// It models exactly what happens between cycles -- the placed orders become resting orders, and the
// client reports their capacity as Reserved and Reusable -- so the budget the next cycle sees is
// the one production would report.
// simulateCycle answers the only question that matters: given the ladder a cycle produced, and
// nothing else changing, does the NEXT cycle want the same thing?
//
// The client's reservation arithmetic is deliberately NOT `size * price`. In production it works in
// engine units (cNGN amount x USDC-per-cNGN) while the old strategy code added capacity back in UI
// units (USDC size x cNGN-per-USDC) -- two different quantities for one reservation, which is what
// made the budget drift. reservedFactor models that disagreement: the client reports a reservation
// that is NOT what `size * price` would give, so any code deriving the budget from OpenOrders
// arrives at the wrong number and the ladder walks.
//
// A simulation that computed Reserved as size*price would be self-consistent and would pass
// whether or not the bug were present. The first version of this test did exactly that and proved
// nothing.
const reservedFactor = 0.97

func simulateCycle(t *testing.T, cfg config.Config, spec exchange.MarketSpec,
	baseTotal, quoteTotal float64, resting []exchange.Order) Result {
	t.Helper()

	var baseReserved, quoteReserved float64
	for _, o := range resting {
		switch o.Side {
		case exchange.SideBuy:
			quoteReserved += o.Size * o.Price * reservedFactor
		case exchange.SideSell:
			baseReserved += o.Size * reservedFactor
		}
	}
	got, err := BuildQuotes(cfg, spec, state.Snapshot{
		Market:              "USDCcNGN-SPOT",
		ExternalAnchorPrice: 1330,
		InventoryByAsset:    map[string]float64{"USDC": 0},
		Positions: map[string]state.AssetPosition{
			"USDC": {Total: baseTotal, Reserved: baseReserved, Reusable: baseReserved,
				Available: maxF(0, baseTotal-baseReserved)},
			"cNGN": {Total: quoteTotal, Reserved: quoteReserved, Reusable: quoteReserved,
				Available: maxF(0, quoteTotal-quoteReserved)},
		},
		OpenOrders: resting,
	})
	if err != nil {
		t.Fatalf("BuildQuotes: %v", err)
	}
	return got
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func quotesToOrders(r Result) []exchange.Order {
	var out []exchange.Order
	for i, q := range r.Bids {
		out = append(out, exchange.Order{ID: "bid", Side: exchange.SideBuy, Price: q.Price, Size: q.Size})
		_ = i
	}
	for _, q := range r.Asks {
		out = append(out, exchange.Order{ID: "ask", Side: exchange.SideSell, Price: q.Price, Size: q.Size})
	}
	return out
}

func stableCfg() config.Config {
	return config.Config{
		OrderSize: 1.2, HalfSpreadBPS: 10, QuoteLevels: 3,
		LevelSpreadStepBPS: 15, LevelSizeMult: 1.2,
		MaxLongInventory: 100, MaxShortInventory: -100,
		MaxNotionalPerSide: 15000,
		USDCCNGNSpotExternalAnchor: config.USDCCNGNSpotExternalAnchorConfig{
			SpreadMultiplier: 1, SizeMultiplier: 1,
		},
	}
}

// The bug this closes. With a flat price and no fills, cycle 2 must want exactly what cycle 1
// placed -- otherwise the bot replaces its own ladder forever, chasing a target that moves only
// because it acted on the previous one.
//
// Before the fix the target drifted ~1.1% per cycle at an unchanged price, and the live diagnostic
// showed 62 of 71 replacements where current_size was the previous cycle's target_size.
func TestFlatPriceAndNoFillsGivesTheSameTargetTwice(t *testing.T) {
	cfg := stableCfg()
	spec := exchange.MarketSpec{
		Symbol: "USDCcNGN-SPOT", BaseAsset: "USDC", QuoteAsset: "cNGN",
		TickSize: 0.000001, SizeStep: 0.000001, MinSize: 0.000001,
	}
	// Capital-constrained on purpose: this only drifted when the budget actually bound the size.
	const baseTotal, quoteTotal = 3.0, 5000.0

	first := simulateCycle(t, cfg, spec, baseTotal, quoteTotal, nil)
	second := simulateCycle(t, cfg, spec, baseTotal, quoteTotal, quotesToOrders(first))

	if len(first.Bids) == 0 || len(first.Asks) == 0 {
		t.Fatal("no ladder to compare")
	}
	if len(second.Bids) != len(first.Bids) || len(second.Asks) != len(first.Asks) {
		t.Fatalf("ladder depth changed: bids %d->%d asks %d->%d",
			len(first.Bids), len(second.Bids), len(first.Asks), len(second.Asks))
	}
	for i := range first.Bids {
		if d := first.Bids[i].Size - second.Bids[i].Size; d > 1e-9 || d < -1e-9 {
			t.Errorf("bid level %d size moved %.9f -> %.9f (diff %.9f) with a flat price and no fills",
				i, first.Bids[i].Size, second.Bids[i].Size, d)
		}
	}
	for i := range first.Asks {
		if d := first.Asks[i].Size - second.Asks[i].Size; d > 1e-9 || d < -1e-9 {
			t.Errorf("ask level %d size moved %.9f -> %.9f (diff %.9f) with a flat price and no fills",
				i, first.Asks[i].Size, second.Asks[i].Size, d)
		}
	}
}

// One stable cycle could be luck. Ten shows it is a fixed point, which is what "the size holds
// still" actually means.
func TestTheLadderIsAFixedPointOverTenCycles(t *testing.T) {
	cfg := stableCfg()
	spec := exchange.MarketSpec{
		Symbol: "USDCcNGN-SPOT", BaseAsset: "USDC", QuoteAsset: "cNGN",
		TickSize: 0.000001, SizeStep: 0.000001, MinSize: 0.000001,
	}
	const baseTotal, quoteTotal = 3.0, 5000.0

	prev := simulateCycle(t, cfg, spec, baseTotal, quoteTotal, nil)
	firstBid := prev.Bids[0].Size
	for cycle := 2; cycle <= 10; cycle++ {
		next := simulateCycle(t, cfg, spec, baseTotal, quoteTotal, quotesToOrders(prev))
		if len(next.Bids) != len(prev.Bids) {
			t.Fatalf("cycle %d: ladder depth changed %d -> %d", cycle, len(prev.Bids), len(next.Bids))
		}
		for i := range prev.Bids {
			if d := prev.Bids[i].Size - next.Bids[i].Size; d > 1e-9 || d < -1e-9 {
				t.Fatalf("cycle %d bid level %d drifted %.9f -> %.9f", cycle, i, prev.Bids[i].Size, next.Bids[i].Size)
			}
		}
		prev = next
	}
	if d := prev.Bids[0].Size - firstBid; d > 1e-9 || d < -1e-9 {
		t.Fatalf("best bid size drifted %.9f over ten cycles (%.9f -> %.9f)", d, firstBid, prev.Bids[0].Size)
	}
}
