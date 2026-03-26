package marketdata

import (
	"context"
	"fmt"
	"time"

	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/state"
)

type Loader struct {
	client exchange.Client
	spec   exchange.MarketSpec
	anchor AnchorSource
}

func NewLoader(client exchange.Client, spec exchange.MarketSpec, anchor AnchorSource) *Loader {
	if anchor == nil {
		anchor = NoopAnchorSource{}
	}
	return &Loader{client: client, spec: spec, anchor: anchor}
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
	anchorPrice, err := l.anchor.GetAnchorPrice(ctx, l.spec.Symbol)
	if err != nil {
		return state.Snapshot{}, &LoadError{Stage: "anchor_data", Err: fmt.Errorf("get anchor price: %w", err)}
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
		AnchorPrice:           anchorPrice,
		AnchorSource:          l.anchor.Name(),
	}
	if l.anchor.Name() != "none" {
		snapshot.LastAnchorRefresh = now
	}
	if len(book.Bids) > 0 {
		snapshot.BestBid = book.Bids[0].Price
	}
	if len(book.Asks) > 0 {
		snapshot.BestAsk = book.Asks[0].Price
	}
	for _, balance := range balances {
		snapshot.InventoryByAsset[balance.Asset] = balance.Total
		snapshot.Positions[balance.Asset] = state.AssetPosition{
			Total:     balance.Total,
			Reserved:  balance.Reserved,
			Available: balance.Available,
		}
	}
	return snapshot, nil
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
