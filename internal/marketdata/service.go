package marketdata

import (
	"context"
	"fmt"
	"time"

	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/state"
)

type Loader struct {
	client                    exchange.Client
	spec                      exchange.MarketSpec
	anchor                    AnchorSource
	spotExternal              USDCCNGNSpotExternalAnchor
	spotExternalBootstrapOnly bool
}

func NewLoader(client exchange.Client, spec exchange.MarketSpec, anchor AnchorSource) *Loader {
	if anchor == nil {
		anchor = NoopAnchorSource{}
	}
	return &Loader{client: client, spec: spec, anchor: anchor, spotExternal: NoopUSDCCNGNSpotExternalAnchor{}}
}

func NewLoaderWithSpotExternal(client exchange.Client, spec exchange.MarketSpec, anchor AnchorSource, spotExternal USDCCNGNSpotExternalAnchor, bootstrapOnly bool) *Loader {
	loader := NewLoader(client, spec, anchor)
	if spotExternal != nil {
		loader.spotExternal = spotExternal
	}
	loader.spotExternalBootstrapOnly = bootstrapOnly
	return loader
}

func (l *Loader) Load(ctx context.Context, last state.Snapshot) (state.Snapshot, error) {
	book, err := l.client.GetBook(ctx, l.spec.Symbol)
	if err != nil {
		return state.Snapshot{}, &LoadError{Stage: "exchange_market_data", Err: fmt.Errorf("get book: %w", err)}
	}
	trades, err := l.client.GetTrades(ctx, l.spec.Symbol)
	if err != nil {
		return state.Snapshot{}, &LoadError{Stage: "exchange_market_data", Err: fmt.Errorf("get trades: %w", err)}
	}
	balances, err := l.client.GetBalances(ctx)
	if err != nil {
		return state.Snapshot{}, &LoadError{Stage: "balances", Err: fmt.Errorf("get balances: %w", err)}
	}
	openOrders, err := l.client.ListOpenOrders(ctx, l.spec.Symbol)
	if err != nil {
		return state.Snapshot{}, &LoadError{Stage: "exchange_market_data", Err: fmt.Errorf("list open orders: %w", err)}
	}
	now := time.Now().UTC()

	snapshot := state.Snapshot{
		Market:                l.spec.Symbol,
		InventoryByAsset:      make(map[string]float64, len(balances)),
		Positions:             make(map[string]state.AssetPosition, len(balances)),
		OpenOrders:            openOrders,
		RecentTrades:          trades,
		LastQuoteUpdate:       last.LastQuoteUpdate,
		LastMarketDataRefresh: now,
		LastBalanceRefresh:    now,
	}
	if len(book.Bids) > 0 {
		snapshot.BestBid = book.Bids[0].Price
	}
	if len(book.Asks) > 0 {
		snapshot.BestAsk = book.Asks[0].Price
	}
	snapshot.LocalReferencePrice, snapshot.LocalReferenceSource = localReference(snapshot)
	for _, balance := range balances {
		snapshot.InventoryByAsset[balance.Asset] = balance.Total
		snapshot.Positions[balance.Asset] = state.AssetPosition{
			Total:     balance.Total,
			Reserved:  balance.Reserved,
			Available: balance.Available,
			Reusable:  balance.Reusable,
		}
	}
	if l.spec.Symbol == "USDCcNGN-SPOT" {
		// The book is spot's source of truth: the reference is its mid, else its last trade, and
		// the oracle is only a bootstrap for a venue with neither (strategy.ComputeReferencePrice).
		// So the oracle is deliberately NOT the snapshot's AnchorPrice: that is what the risk
		// layer's anchor-deviation and stale-anchor guards compare against, and an oracle that
		// disagrees with the book would halt the bot and cancel the only liquidity on the venue.
		// It is still polled every cycle so the bootstrap price is warm when the book empties.
		ext := l.spotExternal.Fetch(ctx)
		snapshot.ExternalAnchorRefreshAttempted = ext.RefreshAttempted
		snapshot.ExternalAnchorRefreshFailed = ext.RefreshFailed
		if ext.Present {
			snapshot.ExternalAnchorPrice = ext.Price
			snapshot.LastExternalAnchorRefresh = ext.FetchedAt
		}
		return snapshot, nil
	}

	anchorPrice, err := l.anchor.GetAnchorPrice(ctx, l.spec.Symbol)
	if err != nil {
		return state.Snapshot{}, &LoadError{Stage: "anchor_data", Err: fmt.Errorf("get anchor price: %w", err)}
	}
	snapshot.AnchorPrice = anchorPrice
	snapshot.AnchorSource = l.anchor.Name()
	if l.anchor.Name() != "none" {
		snapshot.LastAnchorRefresh = now
	}
	return snapshot, nil
}

func localReference(snapshot state.Snapshot) (float64, string) {
	if snapshot.BestBid > 0 && snapshot.BestAsk > 0 {
		return (snapshot.BestBid + snapshot.BestAsk) / 2, "book"
	}
	if price, ok := state.ReferenceTradePrice(snapshot); ok {
		return price, "trade"
	}
	return 0, "none"
}

type LoadError struct {
	Stage string
	Err   error
}

func (e *LoadError) Error() string {
	return e.Stage + ": " + e.Err.Error()
}

func (e *LoadError) Unwrap() error {
	return e.Err
}
