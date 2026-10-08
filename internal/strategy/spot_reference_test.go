package strategy

import (
	"testing"
	"time"

	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/marketnames"
	"github.com/numofx/market-maker/internal/state"
)

// The spot reference-price logic -- the external anchor outranking a stale venue print, and the
// venue's last trade standing however old it is -- must be active under every name the venue has
// listed the spot market under, resolved or not, and never for the perp. Asserted, not inferred:
// each case checks the branch's own observable result.
func TestSpotReferenceLogicIsActiveUnderEveryResolvedName(t *testing.T) {
	now := time.Now().UTC()
	oldTrade := []exchange.Trade{{Price: 0.000750, CreatedAt: now.Add(-10 * time.Minute)}}

	cases := []struct {
		name       string
		market     string
		spotMarket string
		wantSpot   bool
	}{
		{"old service: old name resolved", marketnames.SpotLegacy, marketnames.SpotLegacy, true},
		{"new service: canonical name resolved", marketnames.SpotCanonical, marketnames.SpotCanonical, true},
		{"no resolved spot reference: fallback on the old name", marketnames.SpotLegacy, "", true},
		{"no resolved spot reference: fallback on the canonical name", marketnames.SpotCanonical, "", true},
		{"the perp under a resolved spot reference", marketnames.PerpCanonical, marketnames.SpotCanonical, false},
		{"the perp with no resolved spot reference", marketnames.PerpLegacy, "", false},
		{"a future", "USDCcNGN-SEP16-2026", marketnames.SpotCanonical, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := state.Snapshot{
				Market:                tc.market,
				SpotMarket:            tc.spotMarket,
				LastMarketDataRefresh: now,
				RecentTrades:          oldTrade,
				ExternalAnchorPrice:   0.000800,
				Positions:             fundedPositions(),
			}
			if snapshot.IsSpotMarket() != tc.wantSpot {
				t.Fatalf("IsSpotMarket() = %v, want %v", snapshot.IsSpotMarket(), tc.wantSpot)
			}

			ref, source := ComputeReferencePrice(snapshot)
			tradePrice, tradeOK := state.ReferenceTradePrice(snapshot)
			if tc.wantSpot {
				// Spot: the external price outranks the old print, and the old print still stands.
				if source != "external" || ref != 0.000800 {
					t.Fatalf("spot reference = %v (%s), want 0.0008 (external)", ref, source)
				}
				if !tradeOK || tradePrice != 0.000750 {
					t.Fatalf("spot ReferenceTradePrice = %v, %v; want the standing last trade", tradePrice, tradeOK)
				}
				return
			}
			// Not spot: the external anchor is ignored and the old print is too stale to stand.
			if source == "external" {
				t.Fatalf("non-spot market used the spot external anchor: %v (%s)", ref, source)
			}
			if tradeOK {
				t.Fatalf("non-spot market let a 10-minute-old print stand: %v", tradePrice)
			}
		})
	}
}
