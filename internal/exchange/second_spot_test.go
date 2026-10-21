package exchange

import (
	"context"
	"testing"
	"time"

	"github.com/numofx/market-maker/internal/marketnames"
)

// secondSpotRow is a spot market that is not cNGN's, as markets-service will list EURC-USDC: direct
// orientation, its own escrow, no order_entry_spec.
func secondSpotRow() map[string]any {
	return map[string]any{
		"market": "EURC-USDC", "base_asset_symbol": "EURC", "quote_asset_symbol": "USDC",
		"asset_address": "0x1111111111111111111111111111111111111111", "sub_id": "0",
		"tick_size": "0.000000000000000001", "amount_step": "1", "amount_scale": "1000000000000000000",
		"taker_fee_bps": 25, "contract_type": "spot",
	}
}

// Regression: SpotMarket was "the first spot market in the map". With a second spot market listed,
// map order handed a cNGN-USDC bot the other market on some starts, and startup refuses that
// mismatch (cmd/mm-bot/main.go), so a routine restart could leave cNGN-USDC unquoted. It must name
// the cNGN spot market on every start, and the cNGN markets must resolve as they did alone.
func TestSpotMarketIsCNGNBesideASecondSpotMarket(t *testing.T) {
	for i := 0; i < 50; i++ {
		srv := listingVenue(t, []map[string]any{
			listingRow(marketnames.SpotCanonical, []string{marketnames.SpotLegacy}, false),
			secondSpotRow(),
			listingRow(marketnames.PerpCanonical, []string{marketnames.PerpLegacy}, true),
		})
		c := newTestClient(t, srv.URL, time.Minute)
		spec, err := c.GetMarket(context.Background(), marketnames.SpotLegacy)
		if err != nil {
			t.Fatalf("start %d: GetMarket(%s): %v", i, marketnames.SpotLegacy, err)
		}
		if spec.Symbol != marketnames.SpotCanonical || !spec.IsSpot() {
			t.Fatalf("start %d: cNGN spot resolved as %+v", i, spec)
		}
		if spot, ok := c.SpotMarket(); !ok || spot != marketnames.SpotCanonical {
			t.Fatalf("start %d: SpotMarket() = %q, %v; want %s", i, spot, ok, marketnames.SpotCanonical)
		}
		if perp, err := c.GetMarket(context.Background(), marketnames.PerpLegacy); err != nil || !perp.IsPerp() {
			t.Fatalf("start %d: cNGN-PERP beside a second spot: %+v %v", i, perp, err)
		}
	}
}
