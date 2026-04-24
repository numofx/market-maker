package strategy

import (
	"math"
	"testing"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/state"
)

func TestBuildQuotes(t *testing.T) {
	spec := exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "USDC", QuoteAsset: "cNGN", TickSize: 0.01, SizeStep: 0.1, MinSize: 0.1}
	cfg := config.Config{
		OrderSize:         10,
		HalfSpreadBPS:     50,
		InventorySkewBPS:  20,
		MaxLongInventory:  100,
		MaxShortInventory: -100,
	}

	tests := []struct {
		name     string
		snapshot state.Snapshot
		wantRef  float64
		wantBid  float64
		wantAsk  float64
	}{
		{
			name: "mid from top of book",
			snapshot: state.Snapshot{
				BestBid:          1490,
				BestAsk:          1510,
				InventoryByAsset: map[string]float64{"USDC": 0},
				Positions: map[string]state.AssetPosition{
					"USDC": {Available: 100},
					"cNGN": {Available: 100000},
				},
			},
			wantRef: 1500,
			wantBid: 1492.5,
			wantAsk: 1507.5,
		},
		{
			name: "fallback to last trade",
			snapshot: state.Snapshot{
				RecentTrades:     []exchange.Trade{{Price: 2000}},
				InventoryByAsset: map[string]float64{"USDC": 0},
				Positions: map[string]state.AssetPosition{
					"USDC": {Available: 100},
					"cNGN": {Available: 100000},
				},
			},
			wantRef: 2000,
			wantBid: 1990,
			wantAsk: 2010,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BuildQuotes(cfg, spec, tt.snapshot)
			if err != nil {
				t.Fatalf("BuildQuotes() error = %v", err)
			}
			assertClose(t, got.ReferencePrice, tt.wantRef)
			assertClose(t, got.Bid.Price, tt.wantBid)
			assertClose(t, got.Ask.Price, tt.wantAsk)
		})
	}
}

func TestInventorySkewBehavior(t *testing.T) {
	spec := exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "USDC", QuoteAsset: "cNGN", TickSize: 0.01, SizeStep: 0.1, MinSize: 0.1}
	cfg := config.Config{
		OrderSize:         10,
		HalfSpreadBPS:     20,
		InventorySkewBPS:  100,
		MaxLongInventory:  100,
		MaxShortInventory: -100,
	}
	neutral, err := BuildQuotes(cfg, spec, state.Snapshot{
		BestBid:          999,
		BestAsk:          1001,
		InventoryByAsset: map[string]float64{"USDC": 0},
		Positions: map[string]state.AssetPosition{
			"USDC": {Available: 100},
			"cNGN": {Available: 100000},
		},
	})
	if err != nil {
		t.Fatalf("BuildQuotes() neutral error = %v", err)
	}

	tests := []struct {
		name      string
		inventory float64
	}{
		{name: "long inventory moves quotes down", inventory: 80},
		{name: "short inventory moves quotes up", inventory: -80},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BuildQuotes(cfg, spec, state.Snapshot{
				BestBid:          999,
				BestAsk:          1001,
				InventoryByAsset: map[string]float64{"USDC": tt.inventory},
				Positions: map[string]state.AssetPosition{
					"USDC": {Available: 100},
					"cNGN": {Available: 100000},
				},
			})
			if err != nil {
				t.Fatalf("BuildQuotes() error = %v", err)
			}
			if tt.inventory > 0 {
				if !(got.Bid.Price < neutral.Bid.Price && got.Ask.Price < neutral.Ask.Price) {
					t.Fatalf("expected lower quotes for long inventory, got bid=%v ask=%v", got.Bid.Price, got.Ask.Price)
				}
				return
			}
			if !(got.Bid.Price > neutral.Bid.Price && got.Ask.Price > neutral.Ask.Price) {
				t.Fatalf("expected higher quotes for short inventory, got bid=%v ask=%v", got.Bid.Price, got.Ask.Price)
			}
		})
	}
}

func TestAvailableBalanceCapsQuoteSize(t *testing.T) {
	spec := exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "USDC", QuoteAsset: "cNGN", TickSize: 0.01, SizeStep: 0.1, MinSize: 0.1}
	cfg := config.Config{
		OrderSize:         10,
		HalfSpreadBPS:     20,
		InventorySkewBPS:  0,
		MaxLongInventory:  100,
		MaxShortInventory: -100,
	}
	got, err := BuildQuotes(cfg, spec, state.Snapshot{
		BestBid:          99,
		BestAsk:          101,
		InventoryByAsset: map[string]float64{"USDC": 0},
		Positions: map[string]state.AssetPosition{
			"USDC": {Available: 1.3},
			"cNGN": {Available: 250},
		},
	})
	if err != nil {
		t.Fatalf("BuildQuotes() error = %v", err)
	}
	assertClose(t, got.Ask.Size, 1.3)
	if got.Bid == nil || got.Bid.Size <= 0 {
		t.Fatal("expected affordable bid")
	}
}

func TestExistingOpenOrdersReuseReservedCapacity(t *testing.T) {
	spec := exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "USDC", QuoteAsset: "cNGN", TickSize: 0.01, SizeStep: 0.1, MinSize: 0.1}
	cfg := config.Config{
		OrderSize:         2,
		HalfSpreadBPS:     50,
		InventorySkewBPS:  0,
		MaxLongInventory:  100,
		MaxShortInventory: -100,
		USDCCNGNSpotExternalAnchor: config.USDCCNGNSpotExternalAnchorConfig{
			SpreadMultiplier: 1,
			SizeMultiplier:   1,
		},
	}
	got, err := BuildQuotes(cfg, spec, state.Snapshot{
		Market:              "USDCcNGN-SPOT",
		ExternalAnchorPrice: 1380,
		InventoryByAsset:    map[string]float64{"USDC": 0},
		Positions:           map[string]state.AssetPosition{"USDC": {Available: 0}, "cNGN": {Available: 0}},
		OpenOrders: []exchange.Order{
			{ID: "bid-1", Side: exchange.SideBuy, Price: 1373.1, Size: 2},
			{ID: "ask-1", Side: exchange.SideSell, Price: 1386.9, Size: 2},
		},
	})
	if err != nil {
		t.Fatalf("BuildQuotes() error = %v", err)
	}
	if got.Bid == nil || got.Ask == nil {
		t.Fatalf("expected both quotes to remain targetable with reusable reserved capacity, got bid=%v ask=%v", got.Bid, got.Ask)
	}
	assertClose(t, got.Bid.Size, 2)
	assertClose(t, got.Ask.Size, 2)
}

func TestQuoteSuppressionReasons(t *testing.T) {
	spec := exchange.MarketSpec{
		Symbol:       "USDCcNGN-SPOT",
		BaseAsset:    "USDC",
		QuoteAsset:   "cNGN",
		AssetAddress: "0xe4b6e05b9910ab08a947a20faecc4524bf8a7f7e",
		QuoteAddress: "0x1917960763bf3a0dfa10a05f0a112e828c1a934f",
		TickSize:     0.01,
		SizeStep:     0.1,
		MinSize:      0.1,
	}
	cfg := config.Config{
		SubaccountID:      "6",
		OrderSize:         5,
		HalfSpreadBPS:     20,
		MaxLongInventory:  100,
		MaxShortInventory: -100,
		USDCCNGNSpotExternalAnchor: config.USDCCNGNSpotExternalAnchorConfig{
			SpreadMultiplier: 2,
			SizeMultiplier:   0.1,
		},
	}

	t.Run("no base inventory suppresses ask", func(t *testing.T) {
		got, err := BuildQuotes(cfg, spec, state.Snapshot{
			Market:              "USDCcNGN-SPOT",
			ExternalAnchorPrice: 1353.0884,
			InventoryByAsset:    map[string]float64{"USDC": 0},
			Positions: map[string]state.AssetPosition{
				"USDC": {Available: 0},
				"cNGN": {Available: 1000},
			},
		})
		if err != nil {
			t.Fatalf("BuildQuotes() error = %v", err)
		}
		if got.Ask != nil {
			t.Fatalf("ask = %#v want nil", got.Ask)
		}
		if got.AskSuppression == nil || got.AskSuppression.Reason != "missing_spot_asset_inventory" {
			t.Fatalf("ask suppression = %#v", got.AskSuppression)
		}
		if got.AskSuppression.SpotAssetAddress != spec.AssetAddress {
			t.Fatalf("spot asset address = %q want %q", got.AskSuppression.SpotAssetAddress, spec.AssetAddress)
		}
	})

	t.Run("reserved base suppresses ask with capacity reason", func(t *testing.T) {
		got, err := BuildQuotes(cfg, spec, state.Snapshot{
			Market:              "USDCcNGN-SPOT",
			ExternalAnchorPrice: 1353.0884,
			InventoryByAsset:    map[string]float64{"USDC": 365.57},
			Positions: map[string]state.AssetPosition{
				"USDC": {Total: 365.57, Reserved: 365.57, Available: 0},
				"cNGN": {Available: 1000},
			},
		})
		if err != nil {
			t.Fatalf("BuildQuotes() error = %v", err)
		}
		if got.Ask != nil {
			t.Fatalf("ask = %#v want nil", got.Ask)
		}
		if got.AskSuppression == nil || got.AskSuppression.Reason != "insufficient_base_capacity" {
			t.Fatalf("ask suppression = %#v", got.AskSuppression)
		}
		if got.AskSuppression.TotalCapacity != 365.57 || got.AskSuppression.ReservedCapacity != 365.57 {
			t.Fatalf("ask capacity = total %v reserved %v", got.AskSuppression.TotalCapacity, got.AskSuppression.ReservedCapacity)
		}
	})

	t.Run("insufficient quote capacity suppresses bid", func(t *testing.T) {
		got, err := BuildQuotes(cfg, spec, state.Snapshot{
			Market:              "USDCcNGN-SPOT",
			ExternalAnchorPrice: 1353.0884,
			InventoryByAsset:    map[string]float64{"USDC": 1},
			Positions: map[string]state.AssetPosition{
				"USDC": {Available: 1},
				"cNGN": {Available: 100},
			},
		})
		if err != nil {
			t.Fatalf("BuildQuotes() error = %v", err)
		}
		if got.Bid != nil {
			t.Fatalf("bid = %#v want nil", got.Bid)
		}
		if got.BidSuppression == nil || got.BidSuppression.Reason != "insufficient_quote_capacity" {
			t.Fatalf("bid suppression = %#v", got.BidSuppression)
		}
		if got.BidSuppression.AvailableCapacity != 100 {
			t.Fatalf("available capacity = %v want 100", got.BidSuppression.AvailableCapacity)
		}
	})

	t.Run("valid anchor and balances produce both sides", func(t *testing.T) {
		got, err := BuildQuotes(cfg, spec, state.Snapshot{
			Market:              "USDCcNGN-SPOT",
			ExternalAnchorPrice: 1353.0884,
			InventoryByAsset:    map[string]float64{"USDC": 5},
			Positions: map[string]state.AssetPosition{
				"USDC": {Available: 5},
				"cNGN": {Available: 5000},
			},
		})
		if err != nil {
			t.Fatalf("BuildQuotes() error = %v", err)
		}
		if got.Bid == nil || got.Ask == nil {
			t.Fatalf("expected bid and ask, got bid=%#v ask=%#v suppressions=%#v/%#v", got.Bid, got.Ask, got.BidSuppression, got.AskSuppression)
		}
	})
}

func TestOperatorModes(t *testing.T) {
	spec := exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "USDC", QuoteAsset: "cNGN", TickSize: 0.01, SizeStep: 0.1, MinSize: 0.1}
	baseCfg := config.Config{
		OrderSize:         10,
		HalfSpreadBPS:     20,
		InventorySkewBPS:  0,
		MaxLongInventory:  100,
		MaxShortInventory: -100,
	}
	snapshot := state.Snapshot{
		BestBid:          99,
		BestAsk:          101,
		InventoryByAsset: map[string]float64{"USDC": 0},
		Positions: map[string]state.AssetPosition{
			"USDC": {Available: 100},
			"cNGN": {Available: 100000},
		},
	}

	tests := []struct {
		name    string
		mode    config.OperatorMode
		wantBid bool
		wantAsk bool
	}{
		{name: "normal", mode: config.ModeNormal, wantBid: true, wantAsk: true},
		{name: "bid only", mode: config.ModeBidOnly, wantBid: true, wantAsk: false},
		{name: "ask only", mode: config.ModeAskOnly, wantBid: false, wantAsk: true},
		{name: "pause", mode: config.ModePause, wantBid: false, wantAsk: false},
		{name: "dry run health", mode: config.ModeDryRunHealth, wantBid: false, wantAsk: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseCfg
			cfg.OperatorMode = tt.mode
			got, err := BuildQuotes(cfg, spec, snapshot)
			if err != nil {
				t.Fatalf("BuildQuotes() error = %v", err)
			}
			if (got.Bid != nil) != tt.wantBid {
				t.Fatalf("bid present = %v want %v", got.Bid != nil, tt.wantBid)
			}
			if (got.Ask != nil) != tt.wantAsk {
				t.Fatalf("ask present = %v want %v", got.Ask != nil, tt.wantAsk)
			}
		})
	}
}

func TestSpotLocalReferencePreferredOverExternal(t *testing.T) {
	spec := exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "USDC", QuoteAsset: "cNGN", TickSize: 0.01, SizeStep: 0.1, MinSize: 0.1}
	cfg := config.Config{
		OrderSize:                  10,
		HalfSpreadBPS:              20,
		MaxLongInventory:           100,
		MaxShortInventory:          -100,
		USDCCNGNSpotExternalAnchor: config.USDCCNGNSpotExternalAnchorConfig{SpreadMultiplier: 2, SizeMultiplier: 0.5},
	}

	tests := []struct {
		name       string
		snapshot   state.Snapshot
		wantRef    float64
		wantSource string
	}{
		{
			name: "book beats external",
			snapshot: state.Snapshot{
				Market:               "USDCcNGN-SPOT",
				BestBid:              1490,
				BestAsk:              1510,
				ExternalAnchorPrice:  1700,
				LocalReferenceSource: "book",
				Positions: map[string]state.AssetPosition{
					"USDC": {Available: 100},
					"cNGN": {Available: 100000},
				},
			},
			wantRef:    1500,
			wantSource: "book",
		},
		{
			name: "trade beats external",
			snapshot: state.Snapshot{
				Market:               "USDCcNGN-SPOT",
				RecentTrades:         []exchange.Trade{{Price: 1550}},
				ExternalAnchorPrice:  1700,
				LocalReferenceSource: "trade",
				Positions: map[string]state.AssetPosition{
					"USDC": {Available: 100},
					"cNGN": {Available: 100000},
				},
			},
			wantRef:    1550,
			wantSource: "trade",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BuildQuotes(cfg, spec, tt.snapshot)
			if err != nil {
				t.Fatalf("BuildQuotes() error = %v", err)
			}
			assertClose(t, got.ReferencePrice, tt.wantRef)
			if got.ReferenceSource != tt.wantSource {
				t.Fatalf("reference source = %q want %q", got.ReferenceSource, tt.wantSource)
			}
		})
	}
}

func TestExternalBootstrapMultipliersOnlyApplyWhenExternalActive(t *testing.T) {
	spec := exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "USDC", QuoteAsset: "cNGN", TickSize: 0.01, SizeStep: 0.1, MinSize: 0.1}
	cfg := config.Config{
		OrderSize:         10,
		HalfSpreadBPS:     20,
		MaxLongInventory:  100,
		MaxShortInventory: -100,
		USDCCNGNSpotExternalAnchor: config.USDCCNGNSpotExternalAnchorConfig{
			SpreadMultiplier: 2,
			SizeMultiplier:   0.5,
		},
	}
	basePositions := map[string]state.AssetPosition{
		"USDC": {Available: 100},
		"cNGN": {Available: 100000},
	}

	local, err := BuildQuotes(cfg, spec, state.Snapshot{
		Market:               "USDCcNGN-SPOT",
		BestBid:              1499,
		BestAsk:              1501,
		LocalReferenceSource: "book",
		Positions:            basePositions,
	})
	if err != nil {
		t.Fatalf("local BuildQuotes() error = %v", err)
	}
	external, err := BuildQuotes(cfg, spec, state.Snapshot{
		Market:              "USDCcNGN-SPOT",
		ExternalAnchorPrice: 1500,
		Positions:           basePositions,
	})
	if err != nil {
		t.Fatalf("external BuildQuotes() error = %v", err)
	}
	if external.ReferenceSource != "external" {
		t.Fatalf("reference source = %q want external", external.ReferenceSource)
	}
	if !(external.Bid.Price < local.Bid.Price && external.Ask.Price > local.Ask.Price) {
		t.Fatalf("expected wider external spread, local bid/ask=%v/%v external=%v/%v", local.Bid.Price, local.Ask.Price, external.Bid.Price, external.Ask.Price)
	}
	assertClose(t, external.Bid.Size, 5)
	assertClose(t, external.Ask.Size, 5)
}

func TestNonSpotMarketsUnchangedAndStillPreferConfiguredAnchor(t *testing.T) {
	spec := exchange.MarketSpec{Symbol: "USDCcNGN-APR30-2026", BaseAsset: "USDC", QuoteAsset: "cNGN", TickSize: 0.01, SizeStep: 0.1, MinSize: 0.1}
	cfg := config.Config{
		OrderSize:         10,
		HalfSpreadBPS:     20,
		MaxLongInventory:  100,
		MaxShortInventory: -100,
	}
	got, err := BuildQuotes(cfg, spec, state.Snapshot{
		Market:      spec.Symbol,
		BestBid:     1499,
		BestAsk:     1501,
		AnchorPrice: 1600,
		Positions: map[string]state.AssetPosition{
			"USDC": {Available: 100},
			"cNGN": {Available: 100000},
		},
	})
	if err != nil {
		t.Fatalf("BuildQuotes() error = %v", err)
	}
	assertClose(t, got.ReferencePrice, 1600)
	if got.ReferenceSource != "none" {
		t.Fatalf("reference source = %q want none for unchanged non-spot anchor path", got.ReferenceSource)
	}
}

func assertClose(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-6 {
		t.Fatalf("got %v want %v", got, want)
	}
}
