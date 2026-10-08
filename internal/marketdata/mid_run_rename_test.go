package marketdata

import (
	"context"
	"errors"
	"testing"

	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/marketnames"
	"github.com/numofx/market-maker/internal/state"
)

// renamingClient answers like the exchange client across the venue's rename: before it, the old
// names resolve; after it, either name resolves to the canonical spec with the old one as alias.
type renamingClient struct {
	exchange.Client
	renamed bool
	spot    exchange.MarketSpec
	perp    exchange.MarketSpec
}

func (c *renamingClient) GetBook(context.Context, string) (exchange.Book, error) {
	return exchange.Book{}, nil
}
func (c *renamingClient) GetTrades(context.Context, string) ([]exchange.Trade, error) {
	return nil, nil
}
func (c *renamingClient) GetBalances(context.Context) ([]exchange.Balance, error) { return nil, nil }
func (c *renamingClient) ListOpenOrders(context.Context, string) ([]exchange.Order, error) {
	return nil, nil
}
func (c *renamingClient) GetMarket(_ context.Context, name string) (exchange.MarketSpec, error) {
	for _, spec := range []exchange.MarketSpec{c.spot, c.perp} {
		if spec.HasName(name) {
			return spec, nil
		}
	}
	return exchange.MarketSpec{}, errors.New("unknown market " + name)
}
func (c *renamingClient) SpotMarket() (string, bool) { return c.spot.Symbol, true }

func (c *renamingClient) rename() {
	c.renamed = true
	c.spot.Aliases = []string{c.spot.Symbol}
	c.spot.Symbol = marketnames.SpotCanonical
	c.perp.Aliases = []string{c.perp.Symbol}
	c.perp.Symbol = marketnames.PerpCanonical
}

// A perp bot that started against the old listing keeps loading its perp state after the venue
// renames the markets under it: the lookup by the startup name still finds the perp, the snapshot
// is still priced off the index, and it is still not the spot market.
func TestPerpLoaderSurvivesARenameMidRun(t *testing.T) {
	perpState := &exchange.PerpState{IndexPrice: 0.00073, MarkPrice: 0.00073, TradingEnabled: true, PositionCapNGN: 1e9}
	client := &renamingClient{
		spot: exchange.MarketSpec{Symbol: marketnames.SpotLegacy, Kind: exchange.MarketKindSpot},
		perp: exchange.MarketSpec{Symbol: marketnames.PerpLegacy, Kind: exchange.MarketKindPerp, Perp: perpState},
	}
	startup, err := client.GetMarket(context.Background(), marketnames.PerpLegacy)
	if err != nil {
		t.Fatal(err)
	}
	loader := NewLoader(client, startup, nil).WithPerpBasis(100)
	if loader.SpotMarket() != marketnames.SpotLegacy {
		t.Fatalf("startup spot reference = %q", loader.SpotMarket())
	}

	check := func(stage string) {
		t.Helper()
		snapshot, err := loader.Load(context.Background(), state.Snapshot{})
		if err != nil {
			t.Fatalf("%s: Load: %v", stage, err)
		}
		if snapshot.Perp == nil || snapshot.Perp.Reference != 0.00073 {
			t.Fatalf("%s: perp snapshot = %+v, want the index reference", stage, snapshot.Perp)
		}
		if snapshot.Market != marketnames.PerpLegacy || snapshot.IsSpotMarket() {
			t.Fatalf("%s: market %q IsSpotMarket=%v", stage, snapshot.Market, snapshot.IsSpotMarket())
		}
	}
	check("before the rename")
	client.rename()
	check("after the rename")

	// And the spot loader, built the same way, still sees its market as spot after the rename.
	spotStartup := exchange.MarketSpec{Symbol: marketnames.SpotLegacy, Kind: exchange.MarketKindSpot}
	spotLoader := NewLoader(client, spotStartup, nil)
	snapshot, err := spotLoader.Load(context.Background(), state.Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.IsSpotMarket() || snapshot.SpotMarket != marketnames.SpotLegacy {
		t.Fatalf("spot loader after the rename: market %q spot reference %q", snapshot.Market, snapshot.SpotMarket)
	}
}
