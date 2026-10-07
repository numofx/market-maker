package strategy

import (
	"math"
	"testing"
	"time"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/state"
)

// freshTradeAt is a fixed market-data timestamp; a trade stamped at the same instant is well within
// state.ReferenceTradeMaxAge, so it counts as a live local reference.
var freshTradeAt = time.Unix(1_700_000_000, 0).UTC()

// A funded spot account: 50,000 cNGN (~36 USDC) and 1,000 USDC, with the cNGN as the inventory
// the way the loader reports it.
func fundedPositions() map[string]state.AssetPosition {
	return map[string]state.AssetPosition{
		"cNGN": {Total: 50_000, Available: 50_000},
		"USDC": {Total: 1_000, Available: 1_000},
	}
}

func TestBuildQuotes(t *testing.T) {
	spec := spotSpec()
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
				BestBid:          0.000720,
				BestAsk:          0.000740,
				InventoryByAsset: map[string]float64{"cNGN": 0},
				Positions:        fundedPositions(),
			},
			wantRef: 0.000730,
			wantBid: 0.000730 * 0.995,
			wantAsk: 0.000730 * 1.005,
		},
		{
			name: "fallback to last trade",
			snapshot: state.Snapshot{
				LastMarketDataRefresh: freshTradeAt,
				RecentTrades:          []exchange.Trade{{Price: 0.000800, CreatedAt: freshTradeAt}},
				InventoryByAsset:      map[string]float64{"cNGN": 0},
				Positions:             fundedPositions(),
			},
			wantRef: 0.000800,
			wantBid: 0.000800 * 0.995,
			wantAsk: 0.000800 * 1.005,
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
			// 10 USDC a rung, as whole cNGN at the reference.
			if want := math.Floor(10 / tt.wantRef); got.Bid.Size != want || got.Ask.Size != want {
				t.Fatalf("sizes %v/%v, want %v cNGN", got.Bid.Size, got.Ask.Size, want)
			}
		})
	}
}

// Inventory is cNGN held. Long cNGN (relative to the USDC limits) leans both prices DOWN in USDC
// per cNGN -- sell cNGN cheaper, bid for it lower -- and short leans up.
func TestInventorySkewBehavior(t *testing.T) {
	spec := spotSpec()
	cfg := config.Config{
		OrderSize:         10,
		HalfSpreadBPS:     20,
		InventorySkewBPS:  100,
		MaxLongInventory:  100,
		MaxShortInventory: -100,
	}
	snapshot := func(inventoryCNGN float64) state.Snapshot {
		return state.Snapshot{
			BestBid:          0.000729,
			BestAsk:          0.000731,
			InventoryByAsset: map[string]float64{"cNGN": inventoryCNGN},
			Positions:        fundedPositions(),
		}
	}
	neutral, err := BuildQuotes(cfg, spec, snapshot(0))
	if err != nil {
		t.Fatalf("BuildQuotes() neutral error = %v", err)
	}

	tests := []struct {
		name      string
		inventory float64
	}{
		// 80 USDC of cNGN at 0.00073 is ~109,600 cNGN.
		{name: "long inventory moves quotes down", inventory: 80 / 0.00073},
		{name: "short inventory moves quotes up", inventory: -80 / 0.00073},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BuildQuotes(cfg, spec, snapshot(tt.inventory))
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

// An ask is capped by the cNGN held; a bid by the USDC held at the bid price.
func TestAvailableBalanceCapsQuoteSize(t *testing.T) {
	spec := spotSpec()
	cfg := config.Config{
		OrderSize:         10,
		HalfSpreadBPS:     20,
		InventorySkewBPS:  0,
		MaxLongInventory:  100,
		MaxShortInventory: -100,
	}
	got, err := BuildQuotes(cfg, spec, state.Snapshot{
		BestBid:          0.000729,
		BestAsk:          0.000731,
		InventoryByAsset: map[string]float64{"cNGN": 0},
		Positions: map[string]state.AssetPosition{
			"cNGN": {Total: 1300, Available: 1300},
			"USDC": {Total: 2.5, Available: 2.5},
		},
	})
	if err != nil {
		t.Fatalf("BuildQuotes() error = %v", err)
	}
	assertClose(t, got.Ask.Size, 1300)
	if got.Bid == nil || got.Bid.Size <= 0 || got.Bid.Size > math.Floor(2.5/got.Bid.Price) {
		t.Fatalf("bid %+v, want one affordable with 2.5 USDC", got.Bid)
	}
}

func TestExistingOpenOrdersReuseReservedCapacity(t *testing.T) {
	spec := spotSpec()
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
	const ref = 1 / 1380.0
	rung := math.Floor(2 / ref) // 2 USDC of cNGN
	got, err := BuildQuotes(cfg, spec, state.Snapshot{
		Market:              "USDCcNGN-SPOT",
		ExternalAnchorPrice: ref,
		InventoryByAsset:    map[string]float64{"cNGN": 0},
		// The bot's own two resting orders hold the whole balance: Available is 0, and the client
		// reports that capacity as Reusable because these orders are cancel-replaced each cycle.
		// The budget is Available + Reusable, so the ladder can still be quoted at full size.
		Positions: map[string]state.AssetPosition{
			"cNGN": {Total: rung, Reserved: rung, Available: 0, Reusable: rung},
			"USDC": {Total: 2, Reserved: 2, Available: 0, Reusable: 2},
		},
		OpenOrders: []exchange.Order{
			{ID: "bid-1", Side: exchange.SideBuy, Price: ref * 0.995, Size: rung},
			{ID: "ask-1", Side: exchange.SideSell, Price: ref * 1.005, Size: rung},
		},
	})
	if err != nil {
		t.Fatalf("BuildQuotes() error = %v", err)
	}
	if got.Bid == nil || got.Ask == nil {
		t.Fatalf("expected both quotes to remain targetable with reusable reserved capacity, got bid=%v ask=%v", got.Bid, got.Ask)
	}
	assertClose(t, got.Ask.Size, rung)
	// 2 USDC buys slightly fewer cNGN at a bid above the reference... and slightly more below it;
	// either way at least the rung the reserved USDC funded, rounded to whole cNGN.
	if got.Bid.Size < rung-3 || got.Bid.Size > rung+3 {
		t.Fatalf("bid size %v, want ~%v", got.Bid.Size, rung)
	}
}

func TestQuoteSuppressionReasons(t *testing.T) {
	spec := spotSpec()
	spec.AssetAddress = "0x9d806fd040a719d27a8e5e77dc5ae0ed1e089493"
	spec.QuoteAddress = "0x364058aff6f36e01505fb2cc870f8b6bd4835e84"
	cfg := config.Config{
		SubaccountID:      "26",
		OrderSize:         5,
		HalfSpreadBPS:     20,
		MaxLongInventory:  100,
		MaxShortInventory: -100,
		USDCCNGNSpotExternalAnchor: config.USDCCNGNSpotExternalAnchorConfig{
			SpreadMultiplier: 2,
			SizeMultiplier:   0.1,
		},
	}
	const ref = 1 / 1353.0884

	t.Run("no base inventory suppresses ask", func(t *testing.T) {
		got, err := BuildQuotes(cfg, spec, state.Snapshot{
			Market:              "USDCcNGN-SPOT",
			ExternalAnchorPrice: ref,
			InventoryByAsset:    map[string]float64{"cNGN": 0},
			Positions: map[string]state.AssetPosition{
				"cNGN": {Total: 0, Available: 0},
				"USDC": {Total: 1000, Available: 1000},
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
		if got.AskSuppression.SpotAssetAddress != spec.AssetAddress || got.AskSuppression.BaseAsset != "cNGN" {
			t.Fatalf("suppression names %q/%q, want the cNGN escrow", got.AskSuppression.SpotAssetAddress, got.AskSuppression.BaseAsset)
		}
	})

	t.Run("reserved base suppresses ask with capacity reason", func(t *testing.T) {
		got, err := BuildQuotes(cfg, spec, state.Snapshot{
			Market:              "USDCcNGN-SPOT",
			ExternalAnchorPrice: ref,
			InventoryByAsset:    map[string]float64{"cNGN": 36557},
			Positions: map[string]state.AssetPosition{
				"cNGN": {Total: 36557, Reserved: 36557, Available: 0},
				"USDC": {Total: 1000, Available: 1000},
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
		if got.AskSuppression.TotalCapacity != 36557 || got.AskSuppression.ReservedCapacity != 36557 {
			t.Fatalf("ask capacity = total %v reserved %v", got.AskSuppression.TotalCapacity, got.AskSuppression.ReservedCapacity)
		}
	})

	t.Run("insufficient quote capacity suppresses bid", func(t *testing.T) {
		// 0.0005 USDC buys under one cNGN: the bid cannot be placed at all.
		got, err := BuildQuotes(cfg, spec, state.Snapshot{
			Market:              "USDCcNGN-SPOT",
			ExternalAnchorPrice: ref,
			InventoryByAsset:    map[string]float64{"cNGN": 1000},
			Positions: map[string]state.AssetPosition{
				"cNGN": {Total: 1000, Available: 1000},
				"USDC": {Total: 0.0005, Available: 0.0005},
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
		if got.BidSuppression.AvailableCapacity != 0.0005 {
			t.Fatalf("available capacity = %v want 0.0005", got.BidSuppression.AvailableCapacity)
		}
	})

	t.Run("valid anchor and balances produce both sides", func(t *testing.T) {
		got, err := BuildQuotes(cfg, spec, state.Snapshot{
			Market:              "USDCcNGN-SPOT",
			ExternalAnchorPrice: ref,
			InventoryByAsset:    map[string]float64{"cNGN": 5000},
			Positions: map[string]state.AssetPosition{
				"cNGN": {Total: 5000, Available: 5000},
				"USDC": {Total: 5, Available: 5},
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
	spec := spotSpec()
	baseCfg := config.Config{
		OrderSize:         10,
		HalfSpreadBPS:     20,
		InventorySkewBPS:  0,
		MaxLongInventory:  100,
		MaxShortInventory: -100,
	}
	snapshot := state.Snapshot{
		BestBid:          0.000729,
		BestAsk:          0.000731,
		InventoryByAsset: map[string]float64{"cNGN": 0},
		Positions:        fundedPositions(),
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
	spec := spotSpec()
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
				BestBid:              0.000720,
				BestAsk:              0.000740,
				ExternalAnchorPrice:  0.000800,
				LocalReferenceSource: "book",
				Positions:            fundedPositions(),
			},
			wantRef:    0.000730,
			wantSource: "book",
		},
		{
			name: "external beats a fresh trade",
			snapshot: state.Snapshot{
				Market:                "USDCcNGN-SPOT",
				LastMarketDataRefresh: freshTradeAt,
				RecentTrades:          []exchange.Trade{{Price: 0.000750, CreatedAt: freshTradeAt}},
				ExternalAnchorPrice:   0.000800,
				LocalReferenceSource:  "trade",
				Positions:             fundedPositions(),
			},
			wantRef:    0.000800,
			wantSource: "external",
		},
		{
			// Without a two-sided book the external fallback price outranks the venue's last trade.
			name: "external beats an old trade",
			snapshot: state.Snapshot{
				Market:                "USDCcNGN-SPOT",
				LastMarketDataRefresh: freshTradeAt,
				RecentTrades:          []exchange.Trade{{Price: 0.000750, CreatedAt: freshTradeAt.Add(-10 * time.Minute)}},
				ExternalAnchorPrice:   0.000800,
				Positions:             fundedPositions(),
			},
			wantRef:    0.000800,
			wantSource: "external",
		},
		{
			// The last trade, however old, when there is no fallback price.
			name: "old trade without external",
			snapshot: state.Snapshot{
				Market:                "USDCcNGN-SPOT",
				LastMarketDataRefresh: freshTradeAt,
				RecentTrades:          []exchange.Trade{{Price: 0.000750, CreatedAt: freshTradeAt.Add(-10 * time.Minute)}},
				Positions:             fundedPositions(),
			},
			wantRef:    0.000750,
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
	spec := spotSpec()
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
	const ref = 0.000750

	local, err := BuildQuotes(cfg, spec, state.Snapshot{
		Market:               "USDCcNGN-SPOT",
		BestBid:              ref - 0.000001,
		BestAsk:              ref + 0.000001,
		LocalReferenceSource: "book",
		Positions:            fundedPositions(),
	})
	if err != nil {
		t.Fatalf("local BuildQuotes() error = %v", err)
	}
	external, err := BuildQuotes(cfg, spec, state.Snapshot{
		Market:              "USDCcNGN-SPOT",
		ExternalAnchorPrice: ref,
		Positions:           fundedPositions(),
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
	// Half of 10 USDC, as whole cNGN at the reference.
	if want := math.Floor(5 / ref); external.Bid.Size != want || external.Ask.Size != want {
		t.Fatalf("external sizes %v/%v, want %v cNGN", external.Bid.Size, external.Ask.Size, want)
	}
}

// The dated futures keep their own orientation and units (cNGN per USDC, contracts); nothing about
// the cNGN markets' reorientation touches them.
func futureSpec() exchange.MarketSpec {
	return exchange.MarketSpec{Symbol: "USDCcNGN-APR30-2026", BaseAsset: "USDC", QuoteAsset: "cNGN", TickSize: 0.01, SizeStep: 0.1, MinSize: 0.1}
}

func TestNonSpotMarketsUnchangedAndStillPreferConfiguredAnchor(t *testing.T) {
	spec := futureSpec()
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
			"USDC": {Total: 100, Available: 100},
			"cNGN": {Total: 100000, Available: 100000},
		},
	})
	if err != nil {
		t.Fatalf("BuildQuotes() error = %v", err)
	}
	assertClose(t, got.ReferencePrice, 1600)
	if got.ReferenceSource != "none" {
		t.Fatalf("reference source = %q want none for unchanged non-spot anchor path", got.ReferenceSource)
	}
	// Contracts, not a USDC conversion.
	assertClose(t, got.Bid.Size, 10)
}

func TestCashMarginedFutureQuotesAskWithoutBaseInventory(t *testing.T) {
	spec := futureSpec()
	cfg := config.Config{
		OrderSize:         10,
		HalfSpreadBPS:     20,
		MaxLongInventory:  100,
		MaxShortInventory: -100,
	}
	// Zero base-asset inventory, but cash (quote) margin is available. Selling a
	// cash-margined future opens a SHORT backed by that cash, so the ask must NOT be
	// suppressed for lacking base inventory (BUG 2).
	got, err := BuildQuotes(cfg, spec, state.Snapshot{
		Market:      spec.Symbol,
		BestBid:     1499,
		BestAsk:     1501,
		AnchorPrice: 1500,
		Positions: map[string]state.AssetPosition{
			"USDC": {Total: 0, Reserved: 0, Available: 0},
			"cNGN": {Total: 100000, Available: 100000},
		},
	})
	if err != nil {
		t.Fatalf("BuildQuotes() error = %v", err)
	}
	if got.Ask == nil {
		t.Fatalf("expected ask for cash-margined future, got suppression = %#v", got.AskSuppression)
	}
	assertClose(t, got.Ask.Size, 10)
	if got.Bid == nil {
		t.Fatalf("expected bid for cash-margined future, got suppression = %#v", got.BidSuppression)
	}
}

// Regression: a cash-margined future's quote size must be bounded by the cash margin budget
// (Total/price), NOT the notional of its own resting orders. The old code added reusableCapacity
// (resting bid size*price) to quoteAvailable, so with the cash "Available" driven to 0 by a large
// resting order the size ballooned to ~restingNotional/price and grew every cycle (observed live:
// 0.014 -> 0.126). The fix uses the stable cash Total.
func TestCashMarginedFutureCapacityDoesNotRunAwayFromRestingOrders(t *testing.T) {
	spec := futureSpec()
	cfg := config.Config{OrderSize: 100, HalfSpreadBPS: 20, MaxLongInventory: 1000, MaxShortInventory: -1000}
	got, err := BuildQuotes(cfg, spec, state.Snapshot{
		Market:      spec.Symbol,
		BestBid:     999,
		BestAsk:     1001,
		AnchorPrice: 1000,
		Positions: map[string]state.AssetPosition{
			"USDC": {Total: 0, Reserved: 0, Available: 0},
			"cNGN": {Total: 2000, Reserved: 2000, Available: 0}, // cash fully reserved by the resting orders
		},
		OpenOrders: []exchange.Order{
			{Side: exchange.SideBuy, Size: 10, Price: 1000}, // notional 10000, 5x the 2000 cash budget
			{Side: exchange.SideSell, Size: 10, Price: 1000},
		},
	})
	if err != nil {
		t.Fatalf("BuildQuotes() error = %v", err)
	}
	// Bounded by cash Total/price (~2), NOT the resting-order notional (~10 under the old bug).
	if got.Bid == nil || got.Bid.Size > 3 {
		t.Fatalf("bid ran away: %#v (want size ~Total/price=2, not resting notional ~10)", got.Bid)
	}
	if got.Ask == nil || got.Ask.Size > 3 {
		t.Fatalf("ask ran away: %#v (want size ~Total/price=2)", got.Ask)
	}
}

func TestCashMarginedFutureAskGatedByShortInventoryLimit(t *testing.T) {
	spec := futureSpec()
	cfg := config.Config{
		OrderSize:         10,
		HalfSpreadBPS:     20,
		MaxLongInventory:  100,
		MaxShortInventory: -5, // already near the short cap
	}
	got, err := BuildQuotes(cfg, spec, state.Snapshot{
		Market:           spec.Symbol,
		AnchorPrice:      1500,
		InventoryByAsset: map[string]float64{"USDC": 0},
		Positions: map[string]state.AssetPosition{
			"USDC": {Total: 0, Available: 0},
			"cNGN": {Total: 100000, Available: 100000},
		},
	})
	if err != nil {
		t.Fatalf("BuildQuotes() error = %v", err)
	}
	// inventory 0, askSize 10 -> would go to -10, below MaxShortInventory -5.
	if got.Ask != nil {
		t.Fatalf("expected ask suppressed by short inventory limit, got %#v", got.Ask)
	}
	if got.AskSuppression == nil || got.AskSuppression.Reason != "max_short_inventory" {
		t.Fatalf("ask suppression = %#v want reason max_short_inventory", got.AskSuppression)
	}
}

func assertClose(t *testing.T, got, want float64) {
	t.Helper()
	tolerance := 1e-6
	if want != 0 && math.Abs(want) < 1 {
		// USDC-per-cNGN prices live around 7e-4; compare to a 1e-9 tick.
		tolerance = 2e-9
	}
	if math.Abs(got-want) > tolerance {
		t.Fatalf("got %v want %v", got, want)
	}
}

// Live on 2026-09-14: the bot held 0.000682 USDC, worth under 1 cNGN, which cannot fund a bid for
// even one whole cNGN. Quoting it failed the whole cycle; now only that side is suppressed and the
// cNGN-funded ask side keeps quoting.
func TestSpotSideWorthLessThanOneCNGNIsSuppressedNotQuoted(t *testing.T) {
	spec := spotSpec()
	cfg := config.Config{
		OrderSize:          1.2,
		HalfSpreadBPS:      10,
		QuoteLevels:        5,
		LevelSpreadStepBPS: 15,
		LevelSizeMult:      1.2,
		MaxNetInventory:    60,
		MaxNotionalPerSide: 15000,
	}
	snapshot := state.Snapshot{
		Market:           "USDCcNGN-SPOT",
		BestBid:          1 / 1370.0,
		BestAsk:          1 / 1333.97,
		InventoryByAsset: map[string]float64{"cNGN": 8980},
		Positions: map[string]state.AssetPosition{
			"cNGN": {Total: 8980, Available: 8980},
			"USDC": {Total: 0.000682, Available: 0.000682},
		},
	}

	got, err := BuildQuotes(cfg, spec, snapshot)
	if err != nil {
		t.Fatalf("BuildQuotes() error = %v", err)
	}
	if got.Bid != nil || len(got.Bids) != 0 {
		t.Fatalf("bids = %+v, want none: 0.000682 USDC buys under 1 cNGN", got.Bids)
	}
	if got.BidSuppression == nil || got.BidSuppression.Reason != "insufficient_quote_capacity" {
		t.Fatalf("bid suppression = %+v, want insufficient_quote_capacity", got.BidSuppression)
	}
	if len(got.Asks) == 0 {
		t.Fatal("asks suppressed too; the funded side must keep quoting")
	}
	for _, ask := range got.Asks {
		if ask.Size < 1 || ask.Size != math.Floor(ask.Size) {
			t.Fatalf("ask %+v is not a whole number of cNGN", ask)
		}
	}
}

func TestMinQuoteSizeIsOneCNGNOnTheCNGNMarkets(t *testing.T) {
	if got := minQuoteSize(spotSpec()); got != 1 {
		t.Fatalf("spot min size = %v, want 1 cNGN", got)
	}
	if got := minQuoteSize(perpSpec()); got != 1 {
		t.Fatalf("perp min size = %v, want 1 cNGN", got)
	}
	future := exchange.MarketSpec{Symbol: "USDCcNGN-SEP16-2026", SizeStep: 0.1, MinSize: 0.1}
	if got := minQuoteSize(future); got != 0.1 {
		t.Fatalf("future min size = %v, want spec min 0.1", got)
	}
}

// The operator's USDC sizes and limits are converted at the reference, once, and only on the
// cNGN markets.
func TestOperatorUnitsConvertAtTheReference(t *testing.T) {
	if got := baseSize(spotSpec(), 40, 0.00073); got != 40/0.00073 {
		t.Fatalf("baseSize = %v, want 40/0.00073", got)
	}
	if got := baseSize(futureSpec(), 40, 1370); got != 40 {
		t.Fatalf("a future's size is contracts, got %v", got)
	}
	cfg := config.Config{MaxLongInventory: 6000, MaxShortInventory: -6000}
	long, short := inventoryLimits(cfg, perpSpec(), 0.00073)
	if long != 6000/0.00073 || short != -6000/0.00073 {
		t.Fatalf("perp limits = %v/%v, want +/-6000 USDC in cNGN", long, short)
	}
	cfg = config.Config{MaxNetInventory: 800}
	long, short = inventoryLimits(cfg, spotSpec(), 0.00073)
	if long != 800/0.00073 || short != -800/0.00073 {
		t.Fatalf("spot net limits = %v/%v", long, short)
	}
	if long, short := inventoryLimits(cfg, spotSpec(), 0); long != 0 || short != 0 {
		t.Fatalf("no reference, no room: %v/%v", long, short)
	}
}
