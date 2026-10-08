package execution

import (
	"testing"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/marketnames"
)

// A ladder the previous image tagged with the pre-rename name is adopted at startup under the
// post-rename listing, not cancelled as "ambiguous_ownership" or "wrong_market". The perp's orders
// are still refused on the spot bot: the client marks them unmanaged (the same IsManagedOrderID),
// so they are "ambiguous_ownership" before the market check is reached.
func TestStartupAdoptsOrdersTaggedWithAPreRenameName(t *testing.T) {
	cfg := config.Config{PostOnlyQuotes: true}
	spec := exchange.MarketSpec{Symbol: marketnames.SpotCanonical, Aliases: []string{marketnames.SpotLegacy}, Kind: exchange.MarketKindSpot}
	client := &mockClient{}

	cases := map[string]string{
		"mm:USDCcNGN-SPOT:buy:41": "",
		"mm:cNGN-USDC:sell:42":    "",
		"mm:cNGN-PERP:buy:43":     "ambiguous_ownership",
		"mm:USDCcNGN-PERP:buy:44": "ambiguous_ownership",
		"validation:45":           "ambiguous_ownership",
	}
	for id, want := range cases {
		side := exchange.SideBuy
		if id == "mm:cNGN-USDC:sell:42" {
			side = exchange.SideSell
		}
		order := exchange.Order{ID: id, Side: side, Managed: spec.IsManagedOrderID(id), PostOnly: true, Price: 0.00073}
		if got := startupRejectReason(cfg, spec, order, client); got != want {
			t.Errorf("%s: reject reason %q, want %q", id, got, want)
		}
	}
}
