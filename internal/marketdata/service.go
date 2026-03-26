package marketdata

import (
	"context"
	"fmt"

	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/state"
)

type Loader struct {
	client exchange.Client
	spec   exchange.MarketSpec
}

func NewLoader(client exchange.Client, spec exchange.MarketSpec) *Loader {
	return &Loader{client: client, spec: spec}
}

func (l *Loader) Load(ctx context.Context, last state.Snapshot) (state.Snapshot, error) {
	book, err := l.client.GetBook(ctx, l.spec.Symbol)
	if err != nil {
		return state.Snapshot{}, fmt.Errorf("get book: %w", err)
	}
	trades, err := l.client.GetTrades(ctx, l.spec.Symbol)
	if err != nil {
		return state.Snapshot{}, fmt.Errorf("get trades: %w", err)
	}
	balances, err := l.client.GetBalances(ctx)
	if err != nil {
		return state.Snapshot{}, fmt.Errorf("get balances: %w", err)
	}
	openOrders, err := l.client.ListOpenOrders(ctx, l.spec.Symbol)
	if err != nil {
		return state.Snapshot{}, fmt.Errorf("list open orders: %w", err)
	}

	snapshot := state.Snapshot{
		Market:           l.spec.Symbol,
		InventoryByAsset: make(map[string]float64, len(balances)),
		Positions:        make(map[string]state.AssetPosition, len(balances)),
		OpenOrders:       openOrders,
		RecentTrades:     trades,
		LastQuoteUpdate:  last.LastQuoteUpdate,
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
