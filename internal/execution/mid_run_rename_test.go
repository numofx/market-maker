package execution

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/marketdata"
	"github.com/numofx/market-maker/internal/marketnames"
	"github.com/numofx/market-maker/internal/metrics"
	"github.com/numofx/market-maker/internal/state"
)

// A spot bot that started against the old listing keeps running when the venue renames the market
// under it: the spot reference-price logic stays active, its resting ladder (tagged under the old
// name) is still its own, and nothing is cancelled or re-placed because of the new name.
func TestSpotBotKeepsQuotingAcrossARenameMidRun(t *testing.T) {
	oldSpec := exchange.MarketSpec{Symbol: marketnames.SpotLegacy, Kind: exchange.MarketKindSpot, BaseAsset: "cNGN", QuoteAsset: "USDC", TickSize: 0.0001, SizeStep: 1, MinSize: 1}
	client := &integrationClient{
		spec: oldSpec,
		balances: []exchange.Balance{
			{Asset: "cNGN", Total: 20000, Available: 20000},
			{Asset: "USDC", Total: 100000, Available: 100000},
		},
	}
	// An empty book and no tape: only the external anchor can price spot. On any other market the
	// bot would have nothing to quote from, so a reference of "external" proves the spot logic ran.
	anchor := &fakeSpotExternalAnchor{quotes: []marketdata.ExternalAnchorQuote{{
		Price: 1 / 1500.0, Present: true, FetchedAt: time.Now().UTC(), RefreshAttempted: true,
	}}}
	cfg := config.Config{
		MarketSymbol:              marketnames.SpotLegacy,
		StateFile:                 filepath.Join(t.TempDir(), "state.json"),
		OrderSize:                 10,
		HalfSpreadBPS:             10,
		MaxLongInventory:          100,
		MaxShortInventory:         -100,
		QuoteRefreshInterval:      0,
		CancelStaleOrderThreshold: 20,
		AdoptSizeTolerance:        0.01,
		USDCCNGNSpotExternalAnchor: config.USDCCNGNSpotExternalAnchorConfig{
			Enabled: true, Provider: "0x", BaseURL: "https://example.invalid/price", ChainID: 8453,
			SellToken: "0xsell", BuyToken: "0xbuy", Amount: "1000000", Timeout: time.Second,
			MaxAge: time.Minute, MaxDeviationBPS: 500, BootstrapOnly: true, SpreadMultiplier: 2, SizeMultiplier: 0.5,
		},
	}
	bot := NewBot(cfg, client, oldSpec, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)), state.NewStore(cfg.StateFile))
	bot.loader = marketdata.NewLoaderWithSpotExternal(client, oldSpec, marketdata.NewAnchorSource(cfg, oldSpec), anchor, true)
	ctx := context.Background()

	// Cycle 1, old listing: the bot quotes off the external anchor and places its ladder.
	if err := bot.RunCycle(ctx); err != nil {
		t.Fatalf("cycle 1: %v", err)
	}
	if bot.snapshot.ReferenceSource != "external" || len(client.placed) != 2 {
		t.Fatalf("cycle 1: reference %q, placements %d", bot.snapshot.ReferenceSource, len(client.placed))
	}
	resting := func(spec exchange.MarketSpec) []exchange.Order {
		out := make([]exchange.Order, 0, len(client.placed))
		for _, req := range client.placed {
			out = append(out, exchange.Order{
				ID: req.OrderID, Market: spec.Symbol, Side: req.Side, Price: req.Price, Size: req.Size,
				Nonce: req.Nonce, Managed: spec.IsManagedOrderID(req.OrderID),
			})
		}
		return out
	}
	client.openOrders = resting(oldSpec)

	// Cycle 2, old listing: the ladder rests at its targets, so nothing moves. The baseline that
	// makes the post-rename cycle meaningful.
	if err := bot.RunCycle(ctx); err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	if len(client.cancelled) != 0 || len(client.placed) != 2 {
		t.Fatalf("cycle 2 moved the ladder: cancels %v, placements %d", client.cancelled, len(client.placed))
	}

	// The venue deploys the rename. From here every response names the market cNGN-USDC, the
	// listing resolves the old name as an alias, and the resting orders come back under the new
	// name with their old tags.
	newSpec := oldSpec
	newSpec.Symbol = marketnames.SpotCanonical
	newSpec.Aliases = []string{marketnames.SpotLegacy}
	client.spec = newSpec
	client.openOrders = resting(newSpec)
	for _, order := range client.openOrders {
		if !order.Managed {
			t.Fatalf("after the rename the venue-side predicate dropped %s", order.ID)
		}
	}

	// Cycle 3, renamed: still spot, still priced off the anchor, ladder untouched.
	if err := bot.RunCycle(ctx); err != nil {
		t.Fatalf("cycle 3: %v", err)
	}
	if !bot.snapshot.IsSpotMarket() || bot.snapshot.ReferenceSource != "external" {
		t.Fatalf("after the rename: IsSpotMarket=%v reference %q", bot.snapshot.IsSpotMarket(), bot.snapshot.ReferenceSource)
	}
	if len(client.cancelled) != 0 || len(client.placed) != 2 {
		t.Fatalf("after the rename the ladder moved: cancels %v, placements %d", client.cancelled, len(client.placed))
	}
	// A quote placed after the rename is tagged with the startup name, which the venue still
	// accepts and the bot still recognises either way.
	identities, err := bot.allocateIdentities()
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	for _, id := range identities[exchange.SideBuy] {
		if !newSpec.IsManagedOrderID(id.OrderID) || !oldSpec.IsManagedOrderID(id.OrderID) {
			t.Fatalf("post-rename order id %s not recognised under both specs", id.OrderID)
		}
	}
}
