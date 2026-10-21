package strategy

import (
	"math"
	"testing"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/marketnames"
	"github.com/numofx/market-maker/internal/state"
)

var eurcSpec = exchange.MarketSpec{
	Symbol: "EURC-USDC", Kind: exchange.MarketKindSpot, BaseAsset: "EURC", QuoteAsset: "USDC",
	TickSize: 1e-18, SizeStep: 1, MinSize: 1, TakerFeeBps: 25,
}

func eurcCfg() config.Config {
	return config.Config{
		OrderSize: 200, HalfSpreadBPS: 20, MaxLongInventory: 100000, MaxShortInventory: -100000,
		QuoteLevels: 1, LevelSizeMult: 1, AnchorSourceType: config.AnchorSourceChainlink, MaxAnchorDeviationBPS: 50,
	}
}

func eurcSnapshot(eurc, usdc float64) state.Snapshot {
	return state.Snapshot{
		Market:           "EURC-USDC",
		SpotMarket:       marketnames.SpotCanonical,
		AnchorPrice:      1.12465583,
		AnchorSource:     "chainlink",
		InventoryByAsset: map[string]float64{"EURC": eurc, "USDC": usdc},
		Positions: map[string]state.AssetPosition{
			"EURC": {Total: eurc, Available: eurc},
			"USDC": {Total: usdc, Available: usdc},
		},
	}
}

// With an empty book EURC-USDC quotes around its Chainlink anchor, in USDC per EURC, sized in whole
// EURC: MM_ORDER_SIZE is USDC per rung, 200 USDC at ~1.1247 is 177.8, so 177 EURC.
func TestEURCUSDCQuotesAroundTheAnchorInWholeEURC(t *testing.T) {
	got, err := BuildQuotes(eurcCfg(), eurcSpec, eurcSnapshot(1000, 1000))
	if err != nil {
		t.Fatalf("BuildQuotes: %v", err)
	}
	if got.ReferencePrice != 1.12465583 || got.ReferenceSource != "anchor" {
		t.Fatalf("reference %v from %q, want the anchor", got.ReferencePrice, got.ReferenceSource)
	}
	if got.Bid == nil || got.Ask == nil {
		t.Fatalf("bid %+v ask %+v (suppressed %+v / %+v)", got.Bid, got.Ask, got.BidSuppression, got.AskSuppression)
	}
	if got.Bid.Price >= 1.12465583 || got.Ask.Price <= 1.12465583 || math.Abs(got.Bid.Price-1.12465583*(1-0.002)) > 1e-12 {
		t.Fatalf("bid %v ask %v around 1.12465583", got.Bid.Price, got.Ask.Price)
	}
	for _, q := range []*Quote{got.Bid, got.Ask} {
		if q.Size != 177 || q.Size != math.Floor(q.Size) {
			t.Fatalf("%s size %v, want 177 whole EURC", q.Side, q.Size)
		}
	}
}

// A bid is capped by available USDC over the bid price, in EURC; an ask by the EURC held. Both
// round down to a whole EURC, and a side under one EURC is suppressed, not sent.
func TestEURCUSDCSizingIsCappedByBalances(t *testing.T) {
	got, err := BuildQuotes(eurcCfg(), eurcSpec, eurcSnapshot(12.7, 50))
	if err != nil {
		t.Fatalf("BuildQuotes: %v", err)
	}
	// 50 USDC / 1.12240652 = 44.5 EURC
	if got.Bid == nil || got.Bid.Size != 44 {
		t.Fatalf("bid %+v, want 44 EURC (50 USDC at the bid)", got.Bid)
	}
	if got.Ask == nil || got.Ask.Size != 12 {
		t.Fatalf("ask %+v, want 12 EURC (12.7 held)", got.Ask)
	}

	got, err = BuildQuotes(eurcCfg(), eurcSpec, eurcSnapshot(0.6, 1))
	if err != nil {
		t.Fatalf("BuildQuotes: %v", err)
	}
	if got.Bid != nil || got.BidSuppression == nil || got.BidSuppression.Reason != "insufficient_quote_capacity" {
		t.Fatalf("bid with 1 USDC: %+v %+v", got.Bid, got.BidSuppression)
	}
	if got.Ask != nil || got.AskSuppression == nil || got.AskSuppression.Reason != "insufficient_base_capacity" {
		t.Fatalf("ask with 0.6 EURC: %+v %+v", got.Ask, got.AskSuppression)
	}
}

// The limits in the market's units: MM_MAX_NOTIONAL_PER_SIDE is base (EURC) per side, the
// inventory limits are quote (USDC) at the reference.
func TestEURCUSDCLimitsAreInTheMarketsUnits(t *testing.T) {
	cfg := eurcCfg()
	cfg.MaxNotionalPerSide = 50 // EURC
	got, err := BuildQuotes(cfg, eurcSpec, eurcSnapshot(1000, 1000))
	if err != nil {
		t.Fatalf("BuildQuotes: %v", err)
	}
	if got.Bid == nil || got.Bid.Size != 50 || got.Ask == nil || got.Ask.Size != 50 {
		t.Fatalf("MM_MAX_NOTIONAL_PER_SIDE=50: bid %+v ask %+v, want 50 EURC each", got.Bid, got.Ask)
	}

	cfg = eurcCfg()
	cfg.MaxNetInventory = 1200 // USDC: 1067 EURC at 1.1247
	got, err = BuildQuotes(cfg, eurcSpec, eurcSnapshot(1000, 1000))
	if err != nil {
		t.Fatalf("BuildQuotes: %v", err)
	}
	if got.Bid != nil || got.BidSuppression == nil || got.BidSuppression.Reason != "max_long_inventory" {
		t.Fatalf("1000 EURC held (1124 USDC) + 177 under a 1200 USDC limit: bid %+v %+v", got.Bid, got.BidSuppression)
	}
	if got.Ask == nil {
		t.Fatal("the ask, which reduces inventory, was suppressed")
	}
}

// Regression: cNGN-USDC still prices off other participants' book first, its external anchor as a
// bootstrap only, never the generic anchor, whatever a EURC bot beside it does.
func TestCNGNUSDCReferenceUnchanged(t *testing.T) {
	spec := exchange.MarketSpec{Symbol: marketnames.SpotCanonical, Kind: exchange.MarketKindSpot, BaseAsset: "cNGN", QuoteAsset: "USDC", TickSize: 1e-18, SizeStep: 1, MinSize: 1}
	snap := state.Snapshot{
		Market: marketnames.SpotCanonical, SpotMarket: marketnames.SpotCanonical,
		BestBid: 0.00072, BestAsk: 0.00074, ExternalAnchorPrice: 0.0008,
	}
	if ref, src := ComputeReferencePrice(snap); ref != 0.00073 || src != "book" {
		t.Fatalf("cNGN-USDC with a book: %v %q", ref, src)
	}
	snap.BestBid, snap.BestAsk = 0, 0
	if ref, src := ComputeReferencePrice(snap); ref != 0.0008 || src != "external" {
		t.Fatalf("cNGN-USDC without a book: %v %q", ref, src)
	}
	got, err := BuildQuotes(config.Config{OrderSize: 40, HalfSpreadBPS: 20, MaxLongInventory: 1e6, MaxShortInventory: -1e6, QuoteLevels: 1, LevelSizeMult: 1}, spec, state.Snapshot{
		Market: marketnames.SpotCanonical, SpotMarket: marketnames.SpotCanonical, BestBid: 0.00072, BestAsk: 0.00074,
		Positions: map[string]state.AssetPosition{"cNGN": {Total: 1e6, Available: 1e6}, "USDC": {Total: 1000, Available: 1000}},
	})
	if err != nil || got.Bid == nil {
		t.Fatalf("cNGN-USDC quotes: %+v %v", got, err)
	}
	// 40 USDC at 0.00073 is 54,794.5 cNGN: whole cNGN, as before.
	if got.Bid.Size != 54794 {
		t.Fatalf("cNGN-USDC bid size %v, want 54794", got.Bid.Size)
	}
}
