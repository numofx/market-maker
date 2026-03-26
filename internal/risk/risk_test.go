package risk

import (
	"testing"
	"time"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/state"
)

func TestEvaluate(t *testing.T) {
	now := time.Now().UTC()
	cfg := config.Config{
		MaxLongInventory:  100,
		MaxShortInventory: -100,
		MinBaseBalance:    10,
		MinQuoteBalance:   1000,
	}
	spec := exchange.MarketSpec{BaseAsset: "USDC", QuoteAsset: "cNGN"}

	tests := []struct {
		name     string
		snapshot state.Snapshot
		halt     bool
	}{
		{
			name: "healthy",
			snapshot: state.Snapshot{
				ReferencePrice:   100,
				InventoryByAsset: map[string]float64{"USDC": 0},
				Positions: map[string]state.AssetPosition{
					"USDC": {Total: 100, Available: 100},
					"cNGN": {Total: 5000, Available: 5000},
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
					"USDC": {Total: 100, Available: 100},
					"cNGN": {Total: 5000, Available: 5000},
				},
			},
			halt: true,
		},
		{
			name: "inventory too long",
			snapshot: state.Snapshot{
				ReferencePrice:   100,
				InventoryByAsset: map[string]float64{"USDC": 200},
				Positions: map[string]state.AssetPosition{
					"USDC": {Total: 200, Available: 100},
					"cNGN": {Total: 5000, Available: 5000},
				},
				LastMarketDataRefresh: now,
				LastBalanceRefresh:    now,
			},
			halt: true,
		},
		{
			name: "quote balance too low",
			snapshot: state.Snapshot{
				ReferencePrice:   100,
				InventoryByAsset: map[string]float64{"USDC": 0},
				Positions: map[string]state.AssetPosition{
					"USDC": {Total: 100, Available: 100},
					"cNGN": {Total: 10, Available: 10},
				},
				LastMarketDataRefresh: now,
				LastBalanceRefresh:    now,
			},
			halt: true,
		},
		{
			name: "stale market data halts",
			snapshot: state.Snapshot{
				ReferencePrice:        100,
				InventoryByAsset:      map[string]float64{"USDC": 0},
				LastMarketDataRefresh: now.Add(-3 * time.Second),
				LastBalanceRefresh:    now,
				Positions: map[string]state.AssetPosition{
					"USDC": {Total: 100, Available: 100},
					"cNGN": {Total: 5000, Available: 5000},
				},
			},
			halt: true,
		},
		{
			name: "stale balances halt",
			snapshot: state.Snapshot{
				ReferencePrice:        100,
				InventoryByAsset:      map[string]float64{"USDC": 0},
				LastMarketDataRefresh: now,
				LastBalanceRefresh:    now.Add(-3 * time.Second),
				Positions: map[string]state.AssetPosition{
					"USDC": {Total: 100, Available: 100},
					"cNGN": {Total: 5000, Available: 5000},
				},
			},
			halt: true,
		},
		{
			name: "anchor deviation halts",
			snapshot: state.Snapshot{
				ReferencePrice:        100,
				LocalReferencePrice:   110,
				AnchorPrice:           100,
				AnchorDeviationBPS:    1000,
				InventoryByAsset:      map[string]float64{"USDC": 0},
				LastMarketDataRefresh: now,
				LastBalanceRefresh:    now,
				Positions: map[string]state.AssetPosition{
					"USDC": {Total: 100, Available: 100},
					"cNGN": {Total: 5000, Available: 5000},
				},
			},
			halt: true,
		},
		{
			name: "stale anchor halts separately",
			snapshot: state.Snapshot{
				ReferencePrice:        100,
				AnchorSource:          "fixed",
				InventoryByAsset:      map[string]float64{"USDC": 0},
				LastMarketDataRefresh: now,
				LastBalanceRefresh:    now,
				LastAnchorRefresh:     now.Add(-3 * time.Second),
				Positions: map[string]state.AssetPosition{
					"USDC": {Total: 100, Available: 100},
					"cNGN": {Total: 5000, Available: 5000},
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
		})
	}
}
