package risk

import (
	"testing"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/state"
)

func TestEvaluate(t *testing.T) {
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
			},
			halt: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Evaluate(cfg, spec, tt.snapshot)
			if got.Halt != tt.halt {
				t.Fatalf("Evaluate() halt = %v want %v reason=%s", got.Halt, tt.halt, got.Reason)
			}
		})
	}
}
