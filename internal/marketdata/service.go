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
	// spotMarket is the venue's cNGN spot market as listed (the spot reference market), carried
	// on every snapshot so the strategy keys its spot-only pricing on resolved names.
	spotMarket                string
	anchor                    AnchorSource
	spotExternal              USDCCNGNSpotExternalAnchor
	spotExternalBootstrapOnly bool
	perpMaxBasisBPS           float64
}

// WithPerpBasis sets how far from the index the perp reference may sit (MM_PERP_MAX_BASIS_BPS).
func (l *Loader) WithPerpBasis(bps float64) *Loader {
	l.perpMaxBasisBPS = bps
	return l
}

func NewLoader(client exchange.Client, spec exchange.MarketSpec, anchor AnchorSource) *Loader {
	if anchor == nil {
		anchor = NoopAnchorSource{}
	}
	return &Loader{client: client, spec: spec, spotMarket: ResolveSpotMarket(client, spec), anchor: anchor, spotExternal: NoopUSDCCNGNSpotExternalAnchor{}}
}

// SpotMarketResolver is the exchange client that can name the venue's spot market from its listing.
type SpotMarketResolver interface {
	SpotMarket() (string, bool)
}

// ResolveSpotMarket names the spot reference market: the traded market itself when the venue says
// it is spot, else the spot market the venue lists, else nothing.
func ResolveSpotMarket(client exchange.Client, spec exchange.MarketSpec) string {
	if spec.IsSpot() {
		return spec.Symbol
	}
	if resolver, ok := client.(SpotMarketResolver); ok {
		if name, ok := resolver.SpotMarket(); ok {
			return name
		}
	}
	return ""
}

// SpotMarket is the spot reference market this loader stamps on every snapshot.
func (l *Loader) SpotMarket() string { return l.spotMarket }

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
		SpotMarket:            l.spotMarket,
		InventoryByAsset:      make(map[string]float64, len(balances)),
		Positions:             make(map[string]state.AssetPosition, len(balances)),
		OpenOrders:            openOrders,
		RecentTrades:          trades,
		LastQuoteUpdate:       last.LastQuoteUpdate,
		LastMarketDataRefresh: now,
		LastBalanceRefresh:    now,
	}
	snapshot.BestBid, snapshot.BestAsk = othersBestPrices(book, openOrders)
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
	if l.spec.IsPerp() {
		return l.loadPerp(ctx, snapshot)
	}
	if l.spec.IsSpot() {
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

// loadPerp prices the perp off its index, in USDC per cNGN (the exchange client has already read it
// through the venue's presentation). The venue publishes the index (the rate-picker sources' TWAP)
// and a mark that is the book mid clamped to index +/- 200bps; the bot follows other traders'
// two-sided mid inside a tighter band, and the index otherwise. The index is also the snapshot's
// anchor, so the stale-anchor guard halts the bot when /v1/markets stops refreshing it.
func (l *Loader) loadPerp(ctx context.Context, snapshot state.Snapshot) (state.Snapshot, error) {
	spec, err := l.client.GetMarket(ctx, l.spec.Symbol)
	if err != nil {
		return state.Snapshot{}, &LoadError{Stage: "perp_state", Err: fmt.Errorf("get market: %w", err)}
	}
	if spec.Perp == nil || spec.Perp.IndexPrice <= 0 {
		return state.Snapshot{}, &LoadError{Stage: "perp_state", Err: fmt.Errorf("%s has no index in /v1/markets (feeds stale?)", spec.Symbol)}
	}
	perp := spec.Perp
	reference, source := PerpReference(snapshot.BestBid, snapshot.BestAsk, perp.IndexPrice, l.perpMaxBasisBPS)
	snapshot.Perp = &state.PerpSnapshot{
		Reference:       reference,
		ReferenceSource: source,
		IndexPrice:      perp.IndexPrice,
		MarkPrice:       perp.MarkPrice,
		TradingEnabled:  perp.TradingEnabled,
		SideRoomNGN:     perp.SideRoomNGN(),
		SideRoomUSD:     perp.SideRoomUSD(),
		MaxLeverage:     perp.MaxLeverage,
	}
	snapshot.AnchorPrice = perp.IndexPrice
	snapshot.AnchorSource = "perp_index"
	snapshot.LastAnchorRefresh = perp.FetchedAt
	return snapshot, nil
}

// PerpReference is other traders' mid when their book is two-sided, clamped to index +/- maxBasisBPS;
// the index otherwise. A clamp rather than a halt: a far-off book is exactly when the market needs
// quotes near fair value, and the halt would cancel them.
func PerpReference(bestBid, bestAsk, index, maxBasisBPS float64) (float64, string) {
	if bestBid <= 0 || bestAsk <= 0 || bestBid >= bestAsk {
		return index, "index"
	}
	mid := (bestBid + bestAsk) / 2
	band := index * maxBasisBPS / 10000
	switch {
	case mid > index+band:
		return index + band, "book_clamped"
	case mid < index-band:
		return index - band, "book_clamped"
	default:
		return mid, "book"
	}
}

// othersBestPrices is the top of book with this bot's own resting orders taken out, so the reference
// is the mid of what OTHER participants quote. Counting its own quotes made a one-sided ladder chase
// the lone order opposite it: each re-quote moved the mid it was priced from. Live on 2026-09-14 the
// bid walked from 1351.99 to 1366.23 against a trader's 1370 ask within two cycles. The open orders
// come from the same Load, and the bot places nothing while loading, so the two agree.
func othersBestPrices(book exchange.Book, openOrders []exchange.Order) (bestBid, bestAsk float64) {
	own := make(map[string]struct{}, len(openOrders))
	for _, order := range openOrders {
		own[order.ID] = struct{}{}
	}
	isOwn := func(level exchange.BookLevel) bool {
		_, ok := own[level.OrderID]
		return level.OrderID != "" && ok
	}
	for _, level := range book.Bids {
		if !isOwn(level) && level.Price > bestBid {
			bestBid = level.Price
		}
	}
	for _, level := range book.Asks {
		if !isOwn(level) && level.Price > 0 && (bestAsk == 0 || level.Price < bestAsk) {
			bestAsk = level.Price
		}
	}
	return bestBid, bestAsk
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
