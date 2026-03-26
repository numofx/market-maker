package execution

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/marketdata"
	"github.com/numofx/market-maker/internal/metrics"
	"github.com/numofx/market-maker/internal/risk"
	"github.com/numofx/market-maker/internal/state"
	"github.com/numofx/market-maker/internal/strategy"
)

type Identity struct {
	OrderID string
	Nonce   string
}

type Bot struct {
	cfg     config.Config
	client  exchange.Client
	spec    exchange.MarketSpec
	loader  *marketdata.Loader
	syncer  *Syncer
	metrics *metrics.Registry
	logger  *slog.Logger
	store   *state.Store

	persisted state.Persistent
	snapshot  state.Snapshot
}

func NewBot(cfg config.Config, client exchange.Client, spec exchange.MarketSpec, m *metrics.Registry, logger *slog.Logger, store *state.Store) *Bot {
	return &Bot{
		cfg:       cfg,
		client:    client,
		spec:      spec,
		loader:    marketdata.NewLoader(client, spec),
		syncer:    NewSyncer(client, spec, cfg, m, logger),
		metrics:   m,
		logger:    logger,
		store:     store,
		persisted: state.Persistent{LastNonceBySide: map[string]uint64{}},
	}
}

func (b *Bot) Initialize(ctx context.Context) error {
	if b.store != nil {
		persisted, err := b.store.Load()
		if err != nil {
			return fmt.Errorf("load bot state: %w", err)
		}
		b.persisted = persisted
	}

	snapshot, err := b.loader.Load(ctx, b.snapshot)
	if err != nil {
		return fmt.Errorf("load startup state: %w", err)
	}
	quotes, err := strategy.BuildQuotes(b.cfg, b.spec, snapshot)
	if err != nil {
		return fmt.Errorf("build startup quotes: %w", err)
	}
	snapshot.ReferencePrice = quotes.ReferencePrice
	result, adoptedOrders, err := ReconcileStartup(ctx, b.client, b.cfg, b.spec, snapshot, quotes, b.logger)
	if err != nil {
		return err
	}
	for range result.CanceledOrderIDs {
		b.metrics.IncCancels()
	}
	snapshot.OpenOrders = adoptedOrders
	b.snapshot = snapshot
	return nil
}

func (b *Bot) RunCycle(ctx context.Context) error {
	snapshot, err := b.loader.Load(ctx, b.snapshot)
	if err != nil {
		b.metrics.IncErrors()
		return err
	}
	quotes, err := strategy.BuildQuotes(b.cfg, b.spec, snapshot)
	if err != nil {
		b.metrics.IncErrors()
		return err
	}
	snapshot.ReferencePrice = quotes.ReferencePrice
	b.metrics.SetLastReferencePrice(snapshot.ReferencePrice)
	b.metrics.SetInventory(b.spec.BaseAsset, snapshot.Inventory(b.spec.BaseAsset))
	b.metrics.SetInventory(b.spec.QuoteAsset, snapshot.Inventory(b.spec.QuoteAsset))

	riskDecision := risk.Evaluate(b.cfg, b.spec, snapshot)
	if riskDecision.Halt {
		b.logger.Warn("quoting halted", "reason", riskDecision.Reason)
		if err := b.syncer.CancelAll(ctx, b.spec.Symbol); err != nil {
			return err
		}
		b.snapshot = snapshot
		return nil
	}

	if time.Since(snapshot.LastQuoteUpdate) < b.cfg.QuoteRefreshInterval && len(snapshot.OpenOrders) > 0 {
		b.logger.Debug("quote refresh not due yet", "last_update", snapshot.LastQuoteUpdate)
		b.snapshot = snapshot
		return nil
	}

	ids, err := b.allocateIdentities()
	if err != nil {
		return err
	}
	b.logger.Info("quote decision", "reference_price", quotes.ReferencePrice, "skew_bps", quotes.SkewBPS, "bid", describeQuote(quotes.Bid), "ask", describeQuote(quotes.Ask))
	changed, err := b.syncer.Sync(ctx, snapshot, quotes, ids)
	if err != nil {
		return err
	}
	if changed {
		snapshot.LastQuoteUpdate = time.Now().UTC()
	}
	b.snapshot = snapshot
	return nil
}

func (b *Bot) allocateIdentities() (map[exchange.Side]Identity, error) {
	base := b.persisted.NextNonceBase
	nowBase := uint64(time.Now().UnixMicro()) * 2
	if nowBase > base {
		base = nowBase
	}
	if base%2 != 0 {
		base++
	}
	ids := map[exchange.Side]Identity{
		exchange.SideBuy:  buildIdentity(b.spec.Symbol, exchange.SideBuy, base),
		exchange.SideSell: buildIdentity(b.spec.Symbol, exchange.SideSell, base+1),
	}
	b.persisted.NextNonceBase = base + 2
	b.persisted.LastNonceBySide[string(exchange.SideBuy)] = base
	b.persisted.LastNonceBySide[string(exchange.SideSell)] = base + 1
	if b.store != nil {
		if err := b.store.Save(b.persisted); err != nil {
			return nil, fmt.Errorf("save bot state: %w", err)
		}
	}
	return ids, nil
}

func buildIdentity(market string, side exchange.Side, nonce uint64) Identity {
	return Identity{
		OrderID: fmt.Sprintf("mm:%s:%s:%d", market, side, nonce),
		Nonce:   fmt.Sprintf("%d", nonce),
	}
}

func describeQuote(q *strategy.Quote) any {
	if q == nil {
		return "none"
	}
	return map[string]any{"side": q.Side, "price": q.Price, "size": q.Size}
}
