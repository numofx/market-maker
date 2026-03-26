package execution

import (
	"context"
	"fmt"
	"log/slog"
	"os"
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

	persisted          state.Persistent
	snapshot           state.Snapshot
	lastReconciliation ReconciliationResult
	startedAt          time.Time
	currentHalted      bool
	haltCount          uint64
	maxQuoteAge        time.Duration
	maxAnchorDeviation float64
	maxNetInventory    float64
}

type RuntimeSummary struct {
	Uptime                     time.Duration
	Halted                     bool
	LastHaltReason             string
	HaltCount                  uint64
	FillsBySide                map[string]uint64
	PartialFills               uint64
	CancelCountsByCategory     map[string]uint64
	OpenBidPresent             bool
	OpenAskPresent             bool
	InventoryByAsset           map[string]float64
	NetInventory               float64
	LiveBidCount               int
	LiveAskCount               int
	ExchangeMarketDataAge      time.Duration
	BalanceAge                 time.Duration
	AnchorAge                  time.Duration
	LocalQuoteAge              time.Duration
	ExchangeQuoteAge           time.Duration
	QuotedSpreadBPS            float64
	MaxObservedQuoteAge        time.Duration
	MaxObservedAnchorDeviation float64
	MaxObservedNetInventory    float64
	OperatorMode               string
}

func NewBot(cfg config.Config, client exchange.Client, spec exchange.MarketSpec, m *metrics.Registry, logger *slog.Logger, store *state.Store) *Bot {
	return &Bot{
		cfg:       cfg,
		client:    client,
		spec:      spec,
		loader:    marketdata.NewLoaderWithSpotExternal(client, spec, marketdata.NewAnchorSource(cfg), marketdata.NewUSDCCNGNSpotExternalAnchor(cfg), cfg.USDCCNGNSpotExternalAnchor.BootstrapOnly),
		syncer:    NewSyncer(client, spec, cfg, m, logger),
		metrics:   m,
		logger:    logger,
		store:     store,
		persisted: state.Persistent{LastNonceBySide: map[string]uint64{}},
		startedAt: time.Now().UTC(),
	}
}

func (b *Bot) Initialize(ctx context.Context) error {
	b.metrics.SetOperatorMode(string(b.cfg.OperatorMode))
	if b.store != nil {
		persisted, err := b.store.Load()
		if err != nil {
			return fmt.Errorf("load bot state: %w", err)
		}
		b.persisted = persisted
	}
	if active, err := b.killSwitchActive(); err != nil {
		return err
	} else if active {
		return b.haltForReason(ctx, "kill switch active", true, cancelCategoryKillSwitch)
	}

	snapshot, err := b.loader.Load(ctx, b.snapshot)
	if err != nil {
		return fmt.Errorf("load startup state: %w", err)
	}
	quotes, err := strategy.BuildQuotes(b.cfg, b.spec, snapshot)
	if err != nil {
		return fmt.Errorf("build startup quotes: %w", err)
	}
	b.applyDerivedState(&snapshot, quotes)
	b.logReferenceSourceTransition(b.snapshot, snapshot)
	b.updateReadiness(snapshot, quotes)
	if b.cfg.OperatorMode == config.ModeDryRunHealth {
		b.setHealthyState(snapshot, "")
		b.recordInventory(snapshot)
		if err := b.savePersistent(); err != nil {
			return err
		}
		b.snapshot = snapshot
		return nil
	}
	result, adoptedOrders, err := ReconcileStartup(ctx, b.client, b.cfg, b.spec, snapshot, quotes, b.logger)
	if err != nil {
		return err
	}
	b.lastReconciliation = result
	b.persisted.LastAdoptedBidOrder = result.AdoptedBidOrderID
	b.persisted.LastAdoptedAskOrder = result.AdoptedAskOrderID
	for range result.CanceledOrderIDs {
		b.metrics.IncCancels()
		b.metrics.IncCancelCategory(cancelCategoryStartupReconcile)
	}
	snapshot.OpenOrders = adoptedOrders
	b.setHealthyState(snapshot, "")
	b.recordInventory(snapshot)
	if err := b.savePersistent(); err != nil {
		return err
	}
	b.snapshot = snapshot
	return nil
}

func (b *Bot) RunCycle(ctx context.Context) error {
	if active, err := b.killSwitchActive(); err != nil {
		return err
	} else if active {
		return b.haltForReason(ctx, "kill switch active", true, cancelCategoryKillSwitch)
	}

	snapshot, err := b.loader.Load(ctx, b.snapshot)
	if err != nil {
		b.metrics.IncErrors()
		return b.handleLoadError(ctx, err)
	}
	b.observeFills(b.snapshot, snapshot)
	quotes, err := strategy.BuildQuotes(b.cfg, b.spec, snapshot)
	if err != nil {
		b.metrics.IncErrors()
		return err
	}
	b.applyDerivedState(&snapshot, quotes)
	b.logReferenceSourceTransition(b.snapshot, snapshot)
	b.updateReadiness(snapshot, quotes)

	riskDecision := risk.Evaluate(b.cfg, b.spec, snapshot)
	if riskDecision.Halt {
		b.snapshot = snapshot
		return b.haltForReason(ctx, riskDecision.Reason, true, cancelCategoryRiskTriggered)
	}
	b.clearDependencyStaleMetrics()
	b.metrics.SetHaltState(false, "")
	b.metrics.SetHealth(true, "")
	b.currentHalted = false

	switch b.cfg.OperatorMode {
	case config.ModePause:
		b.snapshot = snapshot
		return b.haltForReason(ctx, "operator pause active", true, "")
	case config.ModeDryRunHealth:
		b.logger.Info("operator mode active", "mode", b.cfg.OperatorMode, "action", "observe_only")
		b.recordInventory(snapshot)
		if err := b.savePersistent(); err != nil {
			return err
		}
		b.snapshot = snapshot
		return nil
	}

	if time.Since(snapshot.LastQuoteUpdate) < b.cfg.QuoteRefreshInterval && len(snapshot.OpenOrders) > 0 {
		b.logger.Debug("quote refresh not due yet", "last_update", snapshot.LastQuoteUpdate)
		b.recordInventory(snapshot)
		if err := b.savePersistent(); err != nil {
			return err
		}
		b.snapshot = snapshot
		return nil
	}

	ids, err := b.allocateIdentities()
	if err != nil {
		return err
	}
	b.logger.Info("quote decision", "reference_price", quotes.ReferencePrice, "skew_bps", quotes.SkewBPS, "bid", describeQuote(quotes.Bid), "ask", describeQuote(quotes.Ask))
	result, err := b.syncer.Sync(ctx, snapshot, quotes, ids)
	if err != nil {
		if rateErr, ok := err.(*CancelRateLimitError); ok {
			b.logger.Warn("quoting halted", "reason", rateErr.Error(), "mode", b.cfg.OperatorMode)
			b.snapshot = snapshot
			return b.haltForReason(ctx, rateErr.Error(), true, cancelCategoryRiskTriggered)
		}
		return err
	}
	if result.Changed {
		snapshot.LastQuoteUpdate = time.Now().UTC()
	}
	if id := result.PlacedOrderIDs[exchange.SideBuy]; id != "" {
		b.persisted.LastSubmittedBidOrder = id
	}
	if id := result.PlacedOrderIDs[exchange.SideSell]; id != "" {
		b.persisted.LastSubmittedAskOrder = id
	}
	b.recordInventory(snapshot)
	if err := b.savePersistent(); err != nil {
		return err
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
		if err := b.savePersistent(); err != nil {
			return nil, err
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

func (b *Bot) LastReconciliationResult() ReconciliationResult {
	return b.lastReconciliation
}

func (b *Bot) applyDerivedState(snapshot *state.Snapshot, quotes strategy.Result) {
	snapshot.ReferencePrice = quotes.ReferencePrice
	snapshot.ReferenceSource = quotes.ReferenceSource
	snapshot.LocalReferencePrice = quotes.LocalReferencePrice
	snapshot.LocalReferenceSource = quotes.LocalReferenceSource
	snapshot.AnchorPrice = quotes.AnchorPrice
	snapshot.AnchorDeviationBPS = 0
	snapshot.LocalQuoteAge = 0
	snapshot.ExchangeQuoteAge = exchangeObservedQuoteAge(snapshot.OpenOrders)
	if !snapshot.LastQuoteUpdate.IsZero() {
		snapshot.LocalQuoteAge = time.Since(snapshot.LastQuoteUpdate)
	}
	if snapshot.AnchorPrice > 0 && snapshot.LocalReferencePrice > 0 {
		snapshot.AnchorDeviationBPS = mathAbs(snapshot.LocalReferencePrice-snapshot.AnchorPrice) / snapshot.AnchorPrice * 10000
	}
	b.metrics.SetLastReferencePrice(snapshot.ReferencePrice)
	b.metrics.SetReferenceSource(quotes.ReferenceSource)
	b.metrics.SetAnchorPrice(snapshot.AnchorPrice)
	b.metrics.SetAnchorLocalDeviationBPS(snapshot.AnchorDeviationBPS)
	b.metrics.SetInventory(b.spec.BaseAsset, snapshot.Inventory(b.spec.BaseAsset))
	b.metrics.SetInventory(b.spec.QuoteAsset, snapshot.Inventory(b.spec.QuoteAsset))
	b.metrics.SetNetInventory(snapshot.Inventory(b.spec.BaseAsset))
	b.metrics.SetOperatorMode(string(b.cfg.OperatorMode))
	b.metrics.SetQuoteAgeSeconds(snapshot.LocalQuoteAge.Seconds())
	b.metrics.SetExchangeQuoteAgeSeconds(snapshot.ExchangeQuoteAge.Seconds())
	if !snapshot.LastMarketDataRefresh.IsZero() {
		b.metrics.SetLastMarketDataRefresh(float64(snapshot.LastMarketDataRefresh.Unix()))
	}
	if !snapshot.LastBalanceRefresh.IsZero() {
		b.metrics.SetLastBalanceRefresh(float64(snapshot.LastBalanceRefresh.Unix()))
	}
	if !snapshot.LastAnchorRefresh.IsZero() {
		b.metrics.SetLastAnchorRefresh(float64(snapshot.LastAnchorRefresh.Unix()))
	}
	now := time.Now().UTC()
	var mdAge, balAge, anchorAge, externalAge float64
	if !snapshot.LastMarketDataRefresh.IsZero() {
		mdAge = now.Sub(snapshot.LastMarketDataRefresh).Seconds()
	}
	if !snapshot.LastBalanceRefresh.IsZero() {
		balAge = now.Sub(snapshot.LastBalanceRefresh).Seconds()
	}
	if !snapshot.LastAnchorRefresh.IsZero() {
		anchorAge = now.Sub(snapshot.LastAnchorRefresh).Seconds()
	}
	if !snapshot.LastExternalAnchorRefresh.IsZero() {
		externalAge = now.Sub(snapshot.LastExternalAnchorRefresh).Seconds()
	}
	if snapshot.ExternalAnchorRefreshAttempted {
		b.metrics.IncExternalAnchorRefresh(!snapshot.ExternalAnchorRefreshFailed)
	}
	b.metrics.SetExternalAnchor(snapshot.ExternalAnchorPrice > 0, externalAge, snapshot.ExternalAnchorPrice)
	b.metrics.SetFreshnessAges(mdAge, balAge, anchorAge)
	if quotes.Bid != nil && quotes.Ask != nil && quotes.ReferencePrice > 0 {
		b.metrics.SetLiveQuotedSpreadBPS((quotes.Ask.Price - quotes.Bid.Price) / quotes.ReferencePrice * 10000)
	} else {
		b.metrics.SetLiveQuotedSpreadBPS(0)
	}
	if snapshot.LocalQuoteAge > b.maxQuoteAge {
		b.maxQuoteAge = snapshot.LocalQuoteAge
	}
	if snapshot.ExchangeQuoteAge > b.maxQuoteAge {
		b.maxQuoteAge = snapshot.ExchangeQuoteAge
	}
	if snapshot.AnchorDeviationBPS > b.maxAnchorDeviation {
		b.maxAnchorDeviation = snapshot.AnchorDeviationBPS
	}
	netInv := mathAbs(snapshot.Inventory(b.spec.BaseAsset))
	if netInv > b.maxNetInventory {
		b.maxNetInventory = netInv
	}
}

func (b *Bot) logReferenceSourceTransition(prev state.Snapshot, next state.Snapshot) {
	if prev.ReferenceSource == next.ReferenceSource {
		return
	}
	b.logger.Info("reference source changed", "market", b.spec.Symbol, "from", prev.ReferenceSource, "to", next.ReferenceSource)
	if b.spec.Symbol != "USDCcNGN-SPOT" {
		return
	}
	if next.ReferenceSource == "external" {
		b.logger.Info("entered external-anchor bootstrap mode", "market", b.spec.Symbol, "source", b.cfg.USDCCNGNSpotExternalAnchor.Provider, "price", next.ReferencePrice)
		return
	}
	if prev.ReferenceSource == "external" && (next.ReferenceSource == "book" || next.ReferenceSource == "trade") {
		b.logger.Info("exited external-anchor bootstrap mode", "market", b.spec.Symbol, "new_source", next.ReferenceSource, "price", next.ReferencePrice)
	}
}

func (b *Bot) setHealthyState(snapshot state.Snapshot, reason string) {
	b.currentHalted = false
	b.metrics.SetHaltState(false, reason)
	b.metrics.SetHealth(true, reason)
	b.metrics.SetOperatorMode(string(b.cfg.OperatorMode))
	b.metrics.SetAnchorPrice(snapshot.AnchorPrice)
	b.clearDependencyStaleMetrics()
}

func (b *Bot) handleLoadError(ctx context.Context, err error) error {
	loadErr, ok := err.(*marketdata.LoadError)
	if !ok {
		return err
	}
	now := time.Now().UTC()
	var haltReason string
	b.clearDependencyStaleMetrics()
	switch loadErr.Stage {
	case "exchange_market_data":
		if b.cfg.StaleMarketDataTimeout > 0 && !b.snapshot.LastMarketDataRefresh.IsZero() && now.Sub(b.snapshot.LastMarketDataRefresh) > b.cfg.StaleMarketDataTimeout {
			haltReason = "exchange market data stale"
			b.metrics.SetDependencyStale("exchange_market_data", true)
		}
	case "balances":
		if b.cfg.StaleBalanceTimeout > 0 && !b.snapshot.LastBalanceRefresh.IsZero() && now.Sub(b.snapshot.LastBalanceRefresh) > b.cfg.StaleBalanceTimeout {
			haltReason = "balances stale"
			b.metrics.SetDependencyStale("balances", true)
		}
	case "anchor_data":
		if b.cfg.StaleAnchorTimeout > 0 && !b.snapshot.LastAnchorRefresh.IsZero() && now.Sub(b.snapshot.LastAnchorRefresh) > b.cfg.StaleAnchorTimeout {
			haltReason = "anchor data stale"
			b.metrics.SetDependencyStale("anchor_data", true)
		}
	}
	if haltReason != "" {
		b.logger.Warn("dependency stale; halting quoting", "reason", haltReason, "error", loadErr.Err)
		return b.haltForReason(ctx, haltReason, true, cancelCategoryRiskTriggered)
	}
	return err
}

func (b *Bot) haltForReason(ctx context.Context, reason string, cancelOrders bool, cancelCategory string) error {
	b.persisted.LastHaltReason = reason
	b.haltCount++
	b.currentHalted = true
	b.recordInventory(b.snapshot)
	b.metrics.SetHaltState(true, reason)
	b.metrics.SetHealth(false, reason)
	b.metrics.SetReadiness(false, reason)
	if cancelOrders {
		if err := b.syncer.CancelAll(ctx, b.spec.Symbol, cancelCategory); err != nil {
			return err
		}
	}
	if err := b.savePersistent(); err != nil {
		return err
	}
	return nil
}

func (b *Bot) savePersistent() error {
	if b.store == nil {
		return nil
	}
	if err := b.store.Save(b.persisted); err != nil {
		return fmt.Errorf("save bot state: %w", err)
	}
	return nil
}

func (b *Bot) recordInventory(snapshot state.Snapshot) {
	b.persisted.LastInventorySnapshot = cloneInventory(snapshot.InventoryByAsset)
}

func cloneInventory(values map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(values))
	for asset, value := range values {
		out[asset] = value
	}
	return out
}

func (b *Bot) killSwitchActive() (bool, error) {
	if b.cfg.KillSwitchFile == "" {
		return false, nil
	}
	_, err := os.Stat(b.cfg.KillSwitchFile)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("check kill switch file: %w", err)
}

func (b *Bot) clearDependencyStaleMetrics() {
	b.metrics.SetDependencyStale("exchange_market_data", false)
	b.metrics.SetDependencyStale("balances", false)
	b.metrics.SetDependencyStale("anchor_data", false)
}

func (b *Bot) updateReadiness(snapshot state.Snapshot, quotes strategy.Result) {
	if b.persisted.LastHaltReason != "" && b.snapshot.ReferencePrice == 0 && snapshot.ReferencePrice == 0 {
		b.metrics.SetReadiness(false, b.persisted.LastHaltReason)
		return
	}
	if snapshot.AnchorSource != "" && snapshot.AnchorSource != "none" && b.cfg.StaleAnchorTimeout > 0 && !snapshot.LastAnchorRefresh.IsZero() && time.Since(snapshot.LastAnchorRefresh) > b.cfg.StaleAnchorTimeout {
		b.metrics.SetReadiness(false, "anchor data stale")
		return
	}
	if b.cfg.StaleBalanceTimeout > 0 && !snapshot.LastBalanceRefresh.IsZero() && time.Since(snapshot.LastBalanceRefresh) > b.cfg.StaleBalanceTimeout {
		b.metrics.SetReadiness(false, "balances stale")
		return
	}
	if b.cfg.StaleMarketDataTimeout > 0 && !snapshot.LastMarketDataRefresh.IsZero() && time.Since(snapshot.LastMarketDataRefresh) > b.cfg.StaleMarketDataTimeout {
		b.metrics.SetReadiness(false, "exchange market data stale")
		return
	}
	if b.cfg.ReadinessMissingQuoteTimeout > 0 && !snapshot.LastQuoteUpdate.IsZero() && time.Since(snapshot.LastQuoteUpdate) > b.cfg.ReadinessMissingQuoteTimeout {
		requiredBid := b.cfg.OperatorMode == config.ModeNormal || b.cfg.OperatorMode == config.ModeBidOnly
		requiredAsk := b.cfg.OperatorMode == config.ModeNormal || b.cfg.OperatorMode == config.ModeAskOnly
		if requiredBid && quotes.Bid != nil && countOrdersBySide(snapshot.OpenOrders, exchange.SideBuy) == 0 {
			b.metrics.SetReadiness(false, "required bid missing too long")
			return
		}
		if requiredAsk && quotes.Ask != nil && countOrdersBySide(snapshot.OpenOrders, exchange.SideSell) == 0 {
			b.metrics.SetReadiness(false, "required ask missing too long")
			return
		}
	}
	if b.metrics != nil {
		b.metrics.SetReadiness(true, "")
	}
}

func countOrdersBySide(orders []exchange.Order, side exchange.Side) int {
	count := 0
	for _, order := range orders {
		if order.Side == side {
			count++
		}
	}
	return count
}

func (b *Bot) Summary() RuntimeSummary {
	summary := RuntimeSummary{
		Uptime:                     time.Since(b.startedAt),
		LastHaltReason:             b.persisted.LastHaltReason,
		HaltCount:                  b.haltCount,
		InventoryByAsset:           cloneInventory(b.snapshot.InventoryByAsset),
		NetInventory:               b.snapshot.Inventory(b.spec.BaseAsset),
		LiveBidCount:               countOrdersBySide(b.snapshot.OpenOrders, exchange.SideBuy),
		LiveAskCount:               countOrdersBySide(b.snapshot.OpenOrders, exchange.SideSell),
		LocalQuoteAge:              b.snapshot.LocalQuoteAge,
		ExchangeQuoteAge:           b.snapshot.ExchangeQuoteAge,
		MaxObservedQuoteAge:        b.maxQuoteAge,
		MaxObservedAnchorDeviation: b.maxAnchorDeviation,
		MaxObservedNetInventory:    b.maxNetInventory,
		OperatorMode:               string(b.cfg.OperatorMode),
		FillsBySide:                map[string]uint64{},
		CancelCountsByCategory:     map[string]uint64{},
	}
	if !b.snapshot.LastMarketDataRefresh.IsZero() {
		summary.ExchangeMarketDataAge = time.Since(b.snapshot.LastMarketDataRefresh)
	}
	if !b.snapshot.LastBalanceRefresh.IsZero() {
		summary.BalanceAge = time.Since(b.snapshot.LastBalanceRefresh)
	}
	if !b.snapshot.LastAnchorRefresh.IsZero() {
		summary.AnchorAge = time.Since(b.snapshot.LastAnchorRefresh)
	}
	for _, order := range b.snapshot.OpenOrders {
		if order.Side == exchange.SideBuy {
			summary.OpenBidPresent = true
		}
		if order.Side == exchange.SideSell {
			summary.OpenAskPresent = true
		}
	}
	summary.Halted = b.currentHalted
	// Metrics-owned counters are mirrored through render helpers via registry snapshot.
	if b.metrics != nil {
		fills, partials, cancels := b.metrics.SnapshotCounters()
		summary.FillsBySide = fills
		summary.PartialFills = partials
		summary.CancelCountsByCategory = cancels
	}
	if b.snapshot.ReferencePrice > 0 && len(b.snapshot.OpenOrders) >= 2 {
		var bid, ask float64
		for _, order := range b.snapshot.OpenOrders {
			if order.Side == exchange.SideBuy && order.Price > bid {
				bid = order.Price
			}
			if order.Side == exchange.SideSell && (ask == 0 || order.Price < ask) {
				ask = order.Price
			}
		}
		if bid > 0 && ask > 0 {
			summary.QuotedSpreadBPS = (ask - bid) / b.snapshot.ReferencePrice * 10000
		}
	}
	return summary
}

func (b *Bot) SoakStatusLine() string {
	s := b.Summary()
	return fmt.Sprintf(
		"state=%s halted=%t inv=%0.6f bids=%d asks=%d fills_buy=%d fills_sell=%d partial_fills=%d cancels=%d md_age=%s bal_age=%s anchor_age=%s",
		s.OperatorMode,
		s.Halted,
		s.NetInventory,
		s.LiveBidCount,
		s.LiveAskCount,
		s.FillsBySide[string(exchange.SideBuy)],
		s.FillsBySide[string(exchange.SideSell)],
		s.PartialFills,
		sumMap(s.CancelCountsByCategory),
		s.ExchangeMarketDataAge.Truncate(time.Second),
		s.BalanceAge.Truncate(time.Second),
		s.AnchorAge.Truncate(time.Second),
	)
}

func (b *Bot) ShutdownSummaryLine() string {
	s := b.Summary()
	return fmt.Sprintf(
		"uptime=%s halts=%d last_halt=%q fills_buy=%d fills_sell=%d partial_fills=%d cancels=%d max_quote_age=%s max_anchor_deviation_bps=%0.4f max_net_inventory=%0.6f",
		s.Uptime.Truncate(time.Second),
		s.HaltCount,
		s.LastHaltReason,
		s.FillsBySide[string(exchange.SideBuy)],
		s.FillsBySide[string(exchange.SideSell)],
		s.PartialFills,
		sumMap(s.CancelCountsByCategory),
		s.MaxObservedQuoteAge.Truncate(time.Second),
		s.MaxObservedAnchorDeviation,
		s.MaxObservedNetInventory,
	)
}

func sumMap(values map[string]uint64) uint64 {
	var out uint64
	for _, v := range values {
		out += v
	}
	return out
}

func (b *Bot) observeFills(previous, current state.Snapshot) {
	if fillsBySide, partials, ok := observeOrderStateFills(previous, current); ok {
		for side, count := range fillsBySide {
			for i := uint64(0); i < count; i++ {
				b.metrics.IncFill(side)
			}
		}
		for i := uint64(0); i < partials; i++ {
			b.metrics.IncPartialFills()
		}
		return
	}
	if fillsBySide := observeTradeFills(previous, current); len(fillsBySide) > 0 {
		for side, count := range fillsBySide {
			for i := uint64(0); i < count; i++ {
				b.metrics.IncFill(side)
			}
		}
		return
	}
	if len(previous.InventoryByAsset) == 0 {
		return
	}
	delta := current.Inventory(b.spec.BaseAsset) - previous.Inventory(b.spec.BaseAsset)
	if delta > 0 {
		b.metrics.IncFill(string(exchange.SideBuy))
	}
	if delta < 0 {
		b.metrics.IncFill(string(exchange.SideSell))
	}
}

func observeOrderStateFills(previous, current state.Snapshot) (map[string]uint64, uint64, bool) {
	prevByID := make(map[string]exchange.Order, len(previous.OpenOrders))
	for _, order := range previous.OpenOrders {
		prevByID[order.ID] = order
	}
	currentByID := make(map[string]exchange.Order, len(current.OpenOrders))
	for _, order := range current.OpenOrders {
		currentByID[order.ID] = order
	}
	fillsBySide := map[string]uint64{}
	var partials uint64
	var haveTruth bool
	for id, prev := range prevByID {
		curr, ok := currentByID[id]
		if !ok {
			fillsBySide[string(prev.Side)]++
			haveTruth = true
			continue
		}
		if curr.Size < prev.Size {
			fillsBySide[string(prev.Side)]++
			partials++
			haveTruth = true
		}
	}
	return fillsBySide, partials, haveTruth
}

func observeTradeFills(previous, current state.Snapshot) map[string]uint64 {
	seen := make(map[int64]struct{}, len(previous.RecentTrades))
	for _, trade := range previous.RecentTrades {
		seen[trade.ID] = struct{}{}
	}
	fillsBySide := map[string]uint64{}
	for _, trade := range current.RecentTrades {
		if _, ok := seen[trade.ID]; ok {
			continue
		}
		switch trade.Side {
		case exchange.SideBuy:
			fillsBySide[string(exchange.SideSell)]++
		case exchange.SideSell:
			fillsBySide[string(exchange.SideBuy)]++
		}
	}
	return fillsBySide
}

func exchangeObservedQuoteAge(orders []exchange.Order) time.Duration {
	now := time.Now().UTC()
	var maxAge time.Duration
	for _, order := range orders {
		if order.CreatedAt.IsZero() {
			continue
		}
		age := now.Sub(order.CreatedAt)
		if age > maxAge {
			maxAge = age
		}
	}
	return maxAge
}

func mathAbs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
