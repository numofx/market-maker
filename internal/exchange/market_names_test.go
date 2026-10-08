package exchange

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/numofx/market-maker/internal/marketnames"
)

const (
	namesSpotAsset = "0x9d806fd040a719d27a8e5e77dc5ae0ed1e089493"
	namesPerpAsset = "0xc74efc8b4808803dbcf439e76fde076d56625b8e"
)

// listingVenue serves one /v1/markets listing: rows as the venue would send them.
func listingVenue(t *testing.T, rows []map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1/markets") {
			_ = json.NewEncoder(w).Encode(rows)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func listingRow(market string, aliases []string, perp bool) map[string]any {
	row := map[string]any{
		"market": market, "base_asset_symbol": "cNGN", "quote_asset_symbol": "USDC",
		"sub_id": "0", "tick_size": "0.000000000000000001", "taker_fee_bps": 25,
	}
	if perp {
		row["contract_type"] = "perpetual"
		row["order_entry_spec"] = SpecCNGNUSDCPerp
		row["asset_address"] = namesPerpAsset
	} else {
		row["contract_type"] = "spot"
		row["order_entry_spec"] = SpecCNGNUSDCSpot
		row["asset_address"] = namesSpotAsset
	}
	if aliases != nil {
		row["aliases"] = aliases
	}
	return row
}

// The three listings the bot must work against: the markets-service before the rename (old names
// only, no aliases field), after it with the aliases it publishes, and after it before it publishes
// them. Under each, MM_MARKET_SYMBOL may be either spelling.
var listings = []struct {
	name          string
	rows          []map[string]any
	spotCanonical string
	perpCanonical string
}{
	{
		name:          "old service: old names, no aliases field",
		rows:          []map[string]any{listingRow(marketnames.SpotLegacy, nil, false), listingRow(marketnames.PerpLegacy, nil, true)},
		spotCanonical: marketnames.SpotLegacy,
		perpCanonical: marketnames.PerpLegacy,
	},
	{
		name: "new service with aliases",
		rows: []map[string]any{
			listingRow(marketnames.SpotCanonical, []string{marketnames.SpotLegacy}, false),
			listingRow(marketnames.PerpCanonical, []string{marketnames.PerpLegacy}, true),
		},
		spotCanonical: marketnames.SpotCanonical,
		perpCanonical: marketnames.PerpCanonical,
	},
	{
		name:          "new service without aliases: the fallback table",
		rows:          []map[string]any{listingRow(marketnames.SpotCanonical, nil, false), listingRow(marketnames.PerpCanonical, nil, true)},
		spotCanonical: marketnames.SpotCanonical,
		perpCanonical: marketnames.PerpCanonical,
	},
}

func TestGetMarketResolvesEitherSpellingUnderEveryListing(t *testing.T) {
	for _, listing := range listings {
		t.Run(listing.name, func(t *testing.T) {
			srv := listingVenue(t, listing.rows)
			c := newTestClient(t, srv.URL, time.Minute)
			ctx := context.Background()

			for _, configured := range []string{marketnames.SpotLegacy, marketnames.SpotCanonical} {
				spec, err := c.GetMarket(ctx, configured)
				if err != nil {
					t.Fatalf("GetMarket(%q): %v", configured, err)
				}
				if spec.Symbol != listing.spotCanonical || !spec.IsSpot() || spec.AssetAddress != namesSpotAsset {
					t.Fatalf("GetMarket(%q) = %q kind=%s asset=%s; want %q spot", configured, spec.Symbol, spec.Kind, spec.AssetAddress, listing.spotCanonical)
				}
				if !spec.HasName(marketnames.SpotLegacy) || !spec.HasName(marketnames.SpotCanonical) || spec.HasName(marketnames.PerpCanonical) {
					t.Fatalf("GetMarket(%q) names = %v", configured, spec.Names())
				}
			}
			for _, configured := range []string{marketnames.PerpLegacy, marketnames.PerpCanonical} {
				spec, err := c.GetMarket(ctx, configured)
				if err != nil {
					t.Fatalf("GetMarket(%q): %v", configured, err)
				}
				if spec.Symbol != listing.perpCanonical || !spec.IsPerp() || spec.AssetAddress != namesPerpAsset {
					t.Fatalf("GetMarket(%q) = %q kind=%s; want %q perp", configured, spec.Symbol, spec.Kind, listing.perpCanonical)
				}
			}

			// The spot reference market resolves from the listing's own statement of what is spot.
			if spot, ok := c.SpotMarket(); !ok || spot != listing.spotCanonical {
				t.Fatalf("SpotMarket() = %q, %v; want %q", spot, ok, listing.spotCanonical)
			}

			// The balances path reads the configured symbol too, under either spelling.
			for _, configured := range []string{marketnames.SpotLegacy, marketnames.SpotCanonical, marketnames.PerpLegacy, marketnames.PerpCanonical} {
				c.cfg.MarketSymbol = configured
				spec, err := c.marketForBalances()
				if err != nil {
					t.Fatalf("marketForBalances with MM_MARKET_SYMBOL=%s: %v", configured, err)
				}
				wantSpot := marketnames.IsSpot(configured)
				if spec.IsSpot() != wantSpot {
					t.Fatalf("marketForBalances with MM_MARKET_SYMBOL=%s resolved %q", configured, spec.Symbol)
				}
			}
		})
	}
}

// A ladder tagged under the pre-rename name is still this bot's under the post-rename listing, and
// the other way round, and the perp's never is the spot's.
func TestManagedOrderIDsAreRecognisedUnderEitherName(t *testing.T) {
	for _, listing := range listings {
		t.Run(listing.name, func(t *testing.T) {
			c := newTestClient(t, listingVenue(t, listing.rows).URL, time.Minute)
			spot, err := c.GetMarket(context.Background(), marketnames.SpotLegacy)
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"mm:USDCcNGN-SPOT:buy:7", "mm:cNGN-USDC:sell:8"} {
				if !spot.IsManagedOrderID(id) {
					t.Errorf("%s not recognised as the spot bot's", id)
				}
			}
			for _, id := range []string{"mm:USDCcNGN-PERP:buy:7", "mm:cNGN-PERP:sell:8", "validation:1", "mm:cNGN-USDC-x:buy:1"} {
				if spot.IsManagedOrderID(id) {
					t.Errorf("%s wrongly recognised as the spot bot's", id)
				}
			}
		})
	}
}

func TestUnknownConfiguredSymbolFailsNamingTheListing(t *testing.T) {
	c := newTestClient(t, listingVenue(t, listings[1].rows).URL, time.Minute)
	_, err := c.GetMarket(context.Background(), "USDCcNGN-SPOTT")
	if err == nil {
		t.Fatal("an unknown symbol resolved")
	}
	for _, want := range []string{`"USDCcNGN-SPOTT"`, marketnames.SpotCanonical, marketnames.SpotLegacy, marketnames.PerpCanonical, marketnames.PerpLegacy} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
	// Exact match only: no case-folding, no prefix.
	for _, name := range []string{"usdccngn-spot", "cngn-usdc", "cNGN", "USDCcNGN"} {
		if _, err := c.GetMarket(context.Background(), name); err == nil {
			t.Errorf("%q resolved inexactly", name)
		}
	}
}

// Aliases the venue lists win over the fallback table; a listed alias that repeats the canonical
// name or another alias is dropped.
func TestListedAliasesTakePrecedenceOverTheFallbackTable(t *testing.T) {
	rows := []map[string]any{listingRow("cNGN-USDC", []string{"cNGN-USDC", "legacy-spot", "legacy-spot", " "}, false)}
	c := newTestClient(t, listingVenue(t, rows).URL, time.Minute)
	spec, err := c.GetMarket(context.Background(), "legacy-spot")
	if err != nil {
		t.Fatalf("listed alias did not resolve: %v", err)
	}
	if !reflect.DeepEqual(spec.Aliases, []string{"legacy-spot"}) {
		t.Fatalf("aliases = %v", spec.Aliases)
	}
	if _, err := c.GetMarket(context.Background(), marketnames.SpotLegacy); err == nil {
		t.Fatal("the fallback alias resolved although the venue listed aliases of its own")
	}
}
