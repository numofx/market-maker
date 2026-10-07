package risk

import (
	"strings"
	"testing"
	"time"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/state"
)

func TestEvaluate(t *testing.T) {
	now := time.Now().UTC()
	// USDC limits on a cNGN market: the cNGN held is valued at the reference price (0.001 USDC
	// per cNGN here, so 200,000 cNGN is 200 USDC). MinBaseBalance is cNGN, MinQuoteBalance USDC.
	cfg := config.Config{
		MaxLongInventory:  100,
		MaxShortInventory: -100,
		MinBaseBalance:    10,
		MinQuoteBalance:   1000,
	}
	spec := exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", Kind: exchange.MarketKindSpot, BaseAsset: "cNGN", QuoteAsset: "USDC"}
	const ref = 0.001

	tests := []struct {
		name     string
		snapshot state.Snapshot
		halt     bool
	}{
		{
			name: "healthy",
			snapshot: state.Snapshot{
				ReferencePrice:   ref,
				InventoryByAsset: map[string]float64{"cNGN": 0},
				Positions: map[string]state.AssetPosition{
					"cNGN": {Total: 5000, Available: 5000},
					"USDC": {Total: 5000, Available: 5000},
				},
				LastMarketDataRefresh: now,
				LastBalanceRefresh:    now,
			},
			halt: false,
		},
		{
			name: "missing reference price",
			snapshot: state.Snapshot{
				Positions: map[string]state.AssetPosition{
					"cNGN": {Total: 5000, Available: 5000},
					"USDC": {Total: 5000, Available: 5000},
				},
			},
			halt: true,
		},
		{
			name: "inventory too long",
			snapshot: state.Snapshot{
				ReferencePrice:   ref,
				InventoryByAsset: map[string]float64{"cNGN": 200_000},
				Positions: map[string]state.AssetPosition{
					"cNGN": {Total: 200_000, Available: 100_000},
					"USDC": {Total: 5000, Available: 5000},
				},
				LastMarketDataRefresh: now,
				LastBalanceRefresh:    now,
			},
			halt: true,
		},
		{
			name: "quote balance too low",
			snapshot: state.Snapshot{
				ReferencePrice:   ref,
				InventoryByAsset: map[string]float64{"cNGN": 0},
				Positions: map[string]state.AssetPosition{
					"cNGN": {Total: 5000, Available: 5000},
					"USDC": {Total: 10, Available: 10},
				},
				LastMarketDataRefresh: now,
				LastBalanceRefresh:    now,
			},
			halt: true,
		},
		{
			name: "stale market data halts",
			snapshot: state.Snapshot{
				ReferencePrice:        ref,
				InventoryByAsset:      map[string]float64{"cNGN": 0},
				LastMarketDataRefresh: now.Add(-3 * time.Second),
				LastBalanceRefresh:    now,
				Positions: map[string]state.AssetPosition{
					"cNGN": {Total: 5000, Available: 5000},
					"USDC": {Total: 5000, Available: 5000},
				},
			},
			halt: true,
		},
		{
			name: "stale balances halt",
			snapshot: state.Snapshot{
				ReferencePrice:        ref,
				InventoryByAsset:      map[string]float64{"cNGN": 0},
				LastMarketDataRefresh: now,
				LastBalanceRefresh:    now.Add(-3 * time.Second),
				Positions: map[string]state.AssetPosition{
					"cNGN": {Total: 5000, Available: 5000},
					"USDC": {Total: 5000, Available: 5000},
				},
			},
			halt: true,
		},
		{
			name: "anchor deviation halts",
			snapshot: state.Snapshot{
				ReferencePrice:        ref,
				LocalReferencePrice:   ref * 1.1,
				AnchorPrice:           ref,
				AnchorDeviationBPS:    1000,
				InventoryByAsset:      map[string]float64{"cNGN": 0},
				LastMarketDataRefresh: now,
				LastBalanceRefresh:    now,
				Positions: map[string]state.AssetPosition{
					"cNGN": {Total: 5000, Available: 5000},
					"USDC": {Total: 5000, Available: 5000},
				},
			},
			halt: true,
		},
		{
			name: "stale anchor halts separately",
			snapshot: state.Snapshot{
				ReferencePrice:        ref,
				AnchorSource:          "fixed",
				InventoryByAsset:      map[string]float64{"cNGN": 0},
				LastMarketDataRefresh: now,
				LastBalanceRefresh:    now,
				LastAnchorRefresh:     now.Add(-3 * time.Second),
				Positions: map[string]state.AssetPosition{
					"cNGN": {Total: 5000, Available: 5000},
					"USDC": {Total: 5000, Available: 5000},
				},
			},
			halt: true,
		},
	}

	cfg.StaleMarketDataTimeout = 2 * time.Second
	cfg.StaleBalanceTimeout = 2 * time.Second
	cfg.StaleAnchorTimeout = 2 * time.Second
	cfg.MaxAnchorDeviationBPS = 500

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Evaluate(cfg, spec, tt.snapshot)
			if got.Halt != tt.halt {
				t.Fatalf("Evaluate() halt = %v want %v reason=%s", got.Halt, tt.halt, got.Reason)
			}
			if tt.name == "stale anchor halts separately" && got.Reason != "anchor data stale" {
				t.Fatalf("reason = %q want anchor data stale", got.Reason)
			}
			if tt.name == "inventory too long" && !strings.Contains(got.Reason, "200.000000 USDC exceeds max long 100") {
				t.Fatalf("reason = %q, want the inventory valued in USDC", got.Reason)
			}
		})
	}
}

// Production's spot limits: MM_MAX_NET_INVENTORY=800 USDC against ~441k cNGN at ~0.00073 (~322
// USDC) does not halt; the same cNGN at a price that values it over 800 USDC does. On the perp the
// position is signed cNGN, so a short is checked against the short limit in USDC.
func TestInventoryLimitsAreUSDCOnTheCNGNMarkets(t *testing.T) {
	now := time.Now().UTC()
	snap := func(spec exchange.MarketSpec, cngn, ref float64) state.Snapshot {
		return state.Snapshot{
			ReferencePrice: ref, InventoryByAsset: map[string]float64{spec.BaseAsset: cngn},
			Positions:             map[string]state.AssetPosition{"cNGN": {Total: cngn, Available: cngn}, "USDC": {Total: 300, Available: 300}},
			LastMarketDataRefresh: now, LastBalanceRefresh: now,
		}
	}
	spot := exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", Kind: exchange.MarketKindSpot, BaseAsset: "cNGN", QuoteAsset: "USDC"}
	cfg := config.Config{MaxNetInventory: 800}
	if d := Evaluate(cfg, spot, snap(spot, 441_000, 0.00073)); d.Halt {
		t.Fatalf("441k cNGN at 0.00073 (~322 USDC) halted: %s", d.Reason)
	}
	if d := Evaluate(cfg, spot, snap(spot, 441_000, 0.002)); !d.Halt {
		t.Fatal("441k cNGN at 0.002 (882 USDC) must halt against an 800 USDC cap")
	}
	perp := exchange.MarketSpec{Symbol: "USDCcNGN-PERP", Kind: exchange.MarketKindPerp, BaseAsset: "cNGN", QuoteAsset: "USDC"}
	cfg = config.Config{MaxLongInventory: 6000, MaxShortInventory: -6000}
	if d := Evaluate(cfg, perp, snap(perp, -8_000_000, 0.00073)); d.Halt {
		t.Fatalf("short 8M cNGN (-5,840 USDC) halted: %s", d.Reason)
	}
	if d := Evaluate(cfg, perp, snap(perp, -8_500_000, 0.00073)); !d.Halt || !strings.Contains(d.Reason, "max short") {
		t.Fatalf("short 8.5M cNGN (-6,205 USDC) must halt against -6000: %+v", d)
	}
	// MM_MAX_NOTIONAL_PER_SIDE is a cNGN amount: an order's own size, not price x size.
	cfg = config.Config{MaxNotionalPerSide: 450_000}
	s := snap(spot, 0, 0.00073)
	s.OpenOrders = []exchange.Order{{Side: exchange.SideBuy, Price: 0.00073, Size: 113_000}}
	if d := Evaluate(cfg, spot, s); d.Halt {
		t.Fatalf("a 113k cNGN order halted against a 450k cap: %s", d.Reason)
	}
	s.OpenOrders = []exchange.Order{{Side: exchange.SideBuy, Price: 0.00073, Size: 460_000}}
	if d := Evaluate(cfg, spot, s); !d.Halt {
		t.Fatal("a 460k cNGN order must halt against a 450k cap")
	}
}
