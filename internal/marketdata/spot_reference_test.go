package marketdata

import (
	"context"
	"testing"

	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/marketnames"
)

type listingClient struct {
	exchange.Client
	spot string
}

func (c listingClient) SpotMarket() (string, bool) { return c.spot, c.spot != "" }

type plainClient struct{ exchange.Client }

func (plainClient) GetMarket(context.Context, string) (exchange.MarketSpec, error) {
	return exchange.MarketSpec{}, nil
}

// The loader stamps every snapshot with the spot reference market it resolved: the traded market
// when the venue says it is spot, else the spot market the venue lists, under whichever name.
func TestLoaderResolvesTheSpotReferenceMarketFromTheListing(t *testing.T) {
	cases := []struct {
		name   string
		spec   exchange.MarketSpec
		client exchange.Client
		want   string
	}{
		{"spot under the old name", exchange.MarketSpec{Symbol: marketnames.SpotLegacy, Kind: exchange.MarketKindSpot}, plainClient{}, marketnames.SpotLegacy},
		{"spot under the canonical name", exchange.MarketSpec{Symbol: marketnames.SpotCanonical, Kind: exchange.MarketKindSpot}, plainClient{}, marketnames.SpotCanonical},
		{"perp, venue lists spot canonically", exchange.MarketSpec{Symbol: marketnames.PerpCanonical, Kind: exchange.MarketKindPerp}, listingClient{spot: marketnames.SpotCanonical}, marketnames.SpotCanonical},
		{"perp, venue lists spot under the old name", exchange.MarketSpec{Symbol: marketnames.PerpLegacy, Kind: exchange.MarketKindPerp}, listingClient{spot: marketnames.SpotLegacy}, marketnames.SpotLegacy},
		{"perp, venue lists no spot", exchange.MarketSpec{Symbol: marketnames.PerpCanonical, Kind: exchange.MarketKindPerp}, listingClient{}, ""},
		{"perp, client cannot name it", exchange.MarketSpec{Symbol: marketnames.PerpCanonical, Kind: exchange.MarketKindPerp}, plainClient{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NewLoader(tc.client, tc.spec, nil).SpotMarket(); got != tc.want {
				t.Fatalf("SpotMarket() = %q, want %q", got, tc.want)
			}
		})
	}
}
