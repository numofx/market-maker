package exchange

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/numofx/market-maker/internal/marketnames"
)

// renamingVenue serves a /v1/markets listing that can be swapped while clients keep calling it:
// the markets-service deploying the rename under a running bot.
type renamingVenue struct {
	mu   sync.Mutex
	rows []map[string]any
}

func (v *renamingVenue) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v.mu.Lock()
		rows := v.rows
		v.mu.Unlock()
		_ = json.NewEncoder(w).Encode(rows)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (v *renamingVenue) set(rows []map[string]any) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.rows = rows
}

// The bot starts against the old listing and the venue renames the markets under it. Without a
// restart, the market cached at startup must still resolve, the spot reference must still be
// found, and orders tagged under either name must still be the bot's.
func TestMarketResolutionSurvivesARenameMidRun(t *testing.T) {
	oldRows := []map[string]any{listingRow(marketnames.SpotLegacy, nil, false), listingRow(marketnames.PerpLegacy, nil, true)}
	newListings := map[string][]map[string]any{
		"renamed, aliases published": {
			listingRow(marketnames.SpotCanonical, []string{marketnames.SpotLegacy}, false),
			listingRow(marketnames.PerpCanonical, []string{marketnames.PerpLegacy}, true),
		},
		"renamed, no aliases field": {listingRow(marketnames.SpotCanonical, nil, false), listingRow(marketnames.PerpCanonical, nil, true)},
	}
	bots := []struct{ configured, oldName, newName string }{
		{marketnames.SpotLegacy, marketnames.SpotLegacy, marketnames.SpotCanonical},
		{marketnames.PerpLegacy, marketnames.PerpLegacy, marketnames.PerpCanonical},
	}
	for name, newRows := range newListings {
		for _, bot := range bots {
			t.Run(name+"/"+bot.configured, func(t *testing.T) {
				venue := &renamingVenue{rows: oldRows}
				c := newTestClient(t, venue.serve(t).URL, time.Millisecond)
				c.cfg.MarketSymbol = bot.configured
				ctx := context.Background()

				startup, err := c.GetMarket(ctx, bot.configured)
				if err != nil || startup.Symbol != bot.oldName {
					t.Fatalf("startup GetMarket = %q, %v", startup.Symbol, err)
				}
				if spot, ok := c.SpotMarket(); !ok || spot != marketnames.SpotLegacy {
					t.Fatalf("startup SpotMarket = %q, %v", spot, ok)
				}

				venue.set(newRows)
				time.Sleep(5 * time.Millisecond) // past refreshEvery: the next lookup refetches

				// Every lookup the running bot makes uses the name it resolved at startup.
				refreshed, err := c.GetMarket(ctx, startup.Symbol)
				if err != nil {
					t.Fatalf("GetMarket(%q) after the rename: %v", startup.Symbol, err)
				}
				if refreshed.Symbol != bot.newName || !refreshed.HasName(bot.oldName) || refreshed.AssetAddress != startup.AssetAddress {
					t.Fatalf("after the rename %q resolved to %q (names %v, asset %s)", startup.Symbol, refreshed.Symbol, refreshed.Names(), refreshed.AssetAddress)
				}
				if spec, err := c.marketForBalances(); err != nil || spec.Symbol != bot.newName {
					t.Fatalf("balances market after the rename = %q, %v", spec.Symbol, err)
				}
				if spot, ok := c.SpotMarket(); !ok || spot != marketnames.SpotCanonical {
					t.Fatalf("SpotMarket after the rename = %q, %v", spot, ok)
				}
				// ListOpenOrders marks Managed with exactly this predicate on exactly this spec.
				for _, id := range []string{"mm:" + bot.oldName + ":buy:1", "mm:" + bot.newName + ":sell:2"} {
					if !refreshed.IsManagedOrderID(id) {
						t.Errorf("%s not recognised as this bot's after the rename", id)
					}
				}
				if _, err := c.GetMarket(ctx, "cNGN-SPOT"); err == nil {
					t.Fatal("an unknown name resolved")
				}
			})
		}
	}
}
