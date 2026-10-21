package exchange

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/numofx/market-maker/internal/marketnames"
)

const (
	eurcEscrow      = "0x1111111111111111111111111111111111111111"
	eurcTradeModule = "0xdea968188598ba0e3f58a56c0fdff338c74f699f"
	eurcQuoteCash   = "0xa74e49b4ed7cb176bc02ef4d8a1a3240c9ad4272"
	marginEngine    = "0xa19e296021c9df25b90d96c2a0793cc3d0800758"
)

// eurcRow is EURC-USDC as markets-service lists it: direct orientation, whole-EURC steps signed
// at 1e18, on its own module and cash under the MarginEngine. No order_entry_spec: none of the cNGN
// specs applies to it, and an unknown one would be read the same way.
func eurcRow() map[string]any {
	return map[string]any{
		"market": "EURC-USDC", "base_asset_symbol": "EURC", "quote_asset_symbol": "USDC",
		"asset_address": eurcEscrow, "sub_id": "0", "tick_size": "0.000000000000000001",
		"amount_step": "1", "amount_scale": "1000000000000000000", "taker_fee_bps": 5,
		"contract_type": "spot", "trade_module_address": eurcTradeModule,
		"quote_asset_address": eurcQuoteCash, "margin_manager_address": marginEngine,
	}
}

func venueWithEURC(t *testing.T) *HTTPClient {
	t.Helper()
	srv := listingVenue(t, []map[string]any{
		listingRow(marketnames.SpotCanonical, []string{marketnames.SpotLegacy}, false),
		eurcRow(),
		listingRow(marketnames.PerpCanonical, []string{marketnames.PerpLegacy}, true),
	})
	return newTestClient(t, srv.URL, time.Minute)
}

func TestEURCUSDCResolvesFromTheListingAsADirectSpotMarket(t *testing.T) {
	c := venueWithEURC(t)
	spec, err := c.GetMarket(context.Background(), "EURC-USDC")
	if err != nil {
		t.Fatalf("GetMarket(EURC-USDC): %v", err)
	}
	if !spec.IsSpot() || !spec.IsAnchoredSpot() || spec.IsCNGNSpot() || spec.IsPerp() || !spec.BaseSized() {
		t.Fatalf("EURC-USDC kind %s: spot %v anchored %v cNGN spot %v", spec.Kind, spec.IsSpot(), spec.IsAnchoredSpot(), spec.IsCNGNSpot())
	}
	if spec.Orientation != OrientationEngine || spec.BaseAsset != "EURC" || spec.QuoteAsset != "USDC" {
		t.Fatalf("EURC-USDC orientation %s base %s quote %s; want engine EURC/USDC as listed", spec.Orientation, spec.BaseAsset, spec.QuoteAsset)
	}
	if spec.AssetAddress != eurcEscrow || spec.SizeStep != 1 || spec.MinSize != 1 || spec.TakerFeeBps != 5 {
		t.Fatalf("EURC-USDC asset %s step %v min %v fee %d", spec.AssetAddress, spec.SizeStep, spec.MinSize, spec.TakerFeeBps)
	}
	if len(spec.Aliases) != 0 || !spec.IsManagedOrderID("mm:EURC-USDC:buy:7") || spec.IsManagedOrderID("mm:cNGN-USDC:buy:7") {
		t.Fatalf("EURC-USDC names %v", spec.Names())
	}
	if spec.ListedTradeModule != eurcTradeModule || spec.ListedQuoteAsset != eurcQuoteCash || spec.MarginManager != marginEngine || spec.AmountScale != "1000000000000000000" {
		t.Fatalf("EURC-USDC wiring %+v", spec)
	}
	if got := assetAddressEnvVar(spec); got != "EURC_SPOT_ASSET_ADDRESS" {
		t.Fatalf("asset env label %q", got)
	}
}

// Nothing inverts EURC-USDC, even under a listing that names an inverted cNGN spec for it.
func TestEURCUSDCIsNeverInverted(t *testing.T) {
	row := eurcRow()
	row["order_entry_spec"] = "eurc_usdc_spot_v1"
	srv := listingVenue(t, []map[string]any{row})
	spec, err := newTestClient(t, srv.URL, time.Minute).GetMarket(context.Background(), "EURC-USDC")
	if err != nil {
		t.Fatalf("GetMarket: %v", err)
	}
	if spec.Orientation != OrientationEngine || spec.BaseAsset != "EURC" {
		t.Fatalf("orientation %s base %s", spec.Orientation, spec.BaseAsset)
	}
}

// Regression: with a second spot market listed, the cNGN markets resolve exactly as before, and the
// spot reference market is always cNGN-USDC. It was "the first spot market in the map", which with
// EURC-USDC listed would hand a cNGN-USDC bot the EURC market on some starts and stop it.
func TestCNGNMarketsUnchangedBesideEURCUSDC(t *testing.T) {
	for i := 0; i < 50; i++ {
		c := venueWithEURC(t)
		if _, err := c.GetMarket(context.Background(), marketnames.SpotLegacy); err != nil {
			t.Fatalf("GetMarket: %v", err)
		}
		spot, ok := c.SpotMarket()
		if !ok || spot != marketnames.SpotCanonical {
			t.Fatalf("start %d: SpotMarket() = %q, %v; want %s", i, spot, ok, marketnames.SpotCanonical)
		}
	}
	c := venueWithEURC(t)
	spec, err := c.GetMarket(context.Background(), marketnames.SpotLegacy)
	if err != nil {
		t.Fatalf("GetMarket(%s): %v", marketnames.SpotLegacy, err)
	}
	if spec.Symbol != marketnames.SpotCanonical || !spec.IsCNGNSpot() || spec.IsAnchoredSpot() || spec.Orientation != OrientationEngine ||
		spec.BaseAsset != "cNGN" || spec.QuoteAsset != "USDC" || spec.SizeStep != 1 || assetAddressEnvVar(spec) != "CNGN_SPOT_ASSET_ADDRESS" {
		t.Fatalf("cNGN-USDC beside EURC-USDC: %+v", spec)
	}
	perp, err := c.GetMarket(context.Background(), marketnames.PerpLegacy)
	if err != nil || !perp.IsPerp() || perp.IsSpot() {
		t.Fatalf("cNGN-PERP beside EURC-USDC: %+v %v", perp, err)
	}
}

// The inverted cNGN presentation still swaps on ingest and is still the cNGN spot market.
func TestInvertedCNGNSpotStillSwapsBesideEURC(t *testing.T) {
	row := listingRow(marketnames.SpotLegacy, nil, false)
	row["order_entry_spec"] = SpecUSDCCNGNSpot
	row["base_asset_symbol"], row["quote_asset_symbol"] = "USDC", "cNGN"
	srv := listingVenue(t, []map[string]any{row, eurcRow()})
	spec, err := newTestClient(t, srv.URL, time.Minute).GetMarket(context.Background(), marketnames.SpotLegacy)
	if err != nil {
		t.Fatalf("GetMarket: %v", err)
	}
	if spec.Orientation != OrientationInverted || spec.BaseAsset != "cNGN" || spec.QuoteAsset != "USDC" || !spec.IsCNGNSpot() {
		t.Fatalf("inverted cNGN spot: %+v", spec)
	}
}

func TestSpotWiringErrorHoldsEURCToItsListing(t *testing.T) {
	c := venueWithEURC(t)
	spec, err := c.GetMarket(context.Background(), "EURC-USDC")
	if err != nil {
		t.Fatalf("GetMarket: %v", err)
	}
	if err := spotWiringError(spec, "0xDea968188598BA0E3F58A56C0fdfF338C74F699f", "0xA74E49b4Ed7cb176bc02ef4D8a1A3240C9aD4272"); err != nil {
		t.Fatalf("matching wiring refused: %v", err)
	}
	for name, tc := range map[string]struct {
		mutate             func(*MarketSpec)
		module, quote, msg string
	}{
		"cNGN spot's module":  {func(*MarketSpec) {}, "0x2222222222222222222222222222222222222222", eurcQuoteCash, "MM_TRADE_MODULE_ADDRESS"},
		"another quote asset": {func(*MarketSpec) {}, eurcTradeModule, "0x3333333333333333333333333333333333333333", "quoteAsset"},
		"a 1e15 amount scale": {func(s *MarketSpec) { s.AmountScale = "1000000000000000" }, eurcTradeModule, eurcQuoteCash, "amount_scale"},
		"no escrow":           {func(s *MarketSpec) { s.AssetAddress = "" }, eurcTradeModule, eurcQuoteCash, "asset_address"},
	} {
		s := spec
		tc.mutate(&s)
		err := spotWiringError(s, tc.module, tc.quote)
		if err == nil || !strings.Contains(err.Error(), tc.msg) {
			t.Fatalf("%s: err %v, want one naming %s", name, err, tc.msg)
		}
	}
}

// The cNGN spot market is not held to the anchored-spot wiring: CheckSpotWiring returns before any
// read, so a cNGN-USDC bot starts exactly as it did.
func TestCheckSpotWiringSkipsTheCNGNMarkets(t *testing.T) {
	c := &HTTPClient{}
	for _, spec := range []MarketSpec{
		{Symbol: marketnames.SpotCanonical, Kind: MarketKindSpot, ListedTradeModule: "0x2222222222222222222222222222222222222222"},
		{Symbol: marketnames.PerpCanonical, Kind: MarketKindPerp},
		{Symbol: "USDC-cNGN-31DEC26", Kind: MarketKindFuture},
	} {
		if err := c.CheckSpotWiring(context.Background(), spec); err != nil {
			t.Fatalf("%s: %v", spec.Symbol, err)
		}
	}
}
