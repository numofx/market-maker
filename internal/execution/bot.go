package execution

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/control"
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

type marketAssetValidator interface {
	ValidateMarketAssets(context.Context, exchange.MarketSpec) ([]exchange.AssetCodeCheck, error)
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

	// Operator control. control is nil for a bot built without one -- every test that predates the
	// control API -- and every Controller method is nil-safe, so that bot behaves exactly as before.
	control *control.Controller
	// marketSymbol is spec.Symbol captured at construction. b.spec is rewritten by syncFeeSchedule on
	// the loop, so the control API's cancel path must not read it.
	marketSymbol  string
	initialized   bool
	lastQuotes    strategy.Result
	haltSince     time.Time
	runningSince  time.Time
	killFileSince time.Time
	lastCycleAt   time.Time
	lastCycleErr  string
	// view is the copy of the above that the control API reads from its own goroutines. Everything
	// else on Bot belongs to the quoting loop.
	viewMu sync.Mutex
	view   control.BotView
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
	now := time.Now().UTC()
	b := &Bot{
		cfg:          cfg,
		client:       client,
		spec:         spec,
		loader:       marketdata.NewLoaderWithSpotExternal(client, spec, marketdata.NewAnchorSource(cfg, spec), marketdata.NewUSDCCNGNSpotExternalAnchor(cfg), cfg.USDCCNGNSpotExternalAnchor.BootstrapOnly),
		syncer:       NewSyncer(client, spec, cfg, m, logger),
		metrics:      m,
		logger:       logger,
		store:        store,
		persisted:    state.Persistent{LastNonceBySide: map[string]uint64{}},
		startedAt:    now,
		marketSymbol: spec.Symbol,
		runningSince: now,
	}
	b.publishView()
	return b
}

func (b *Bot) Initialize(ctx context.Context) (err error) {
	defer func() {
		if err == nil {
			b.initialized = true
		}
		b.publishView()
	}()
	b.metrics.SetOperatorMode(string(b.cfg.OperatorMode))
	b.logger.Info("selected market metadata", marketMetadataAttrs(b.spec)...)
	if validator, ok := b.client.(marketAssetValidator); ok {
		if _, err := validator.ValidateMarketAssets(ctx, b.spec); err != nil {
			b.metrics.SetReadiness(false, "token_address_has_no_code")
			return err
		}
	}
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
		b.killFileSince = time.Now().UTC()
		return b.haltForReason(ctx, control.HaltReasonKillSwitchFile, true, cancelCategoryKillSwitch)
	}
	// A kill set through the control API survives the restart (the controller restored it from the
	// state file). Checked before anything is loaded, like the file: a kill must not wait on the
	// exchange being reachable, and a restarting killed bot must not reconcile its way into quoting.
	if b.control.Status().Killed {
		return b.haltForReason(ctx, control.HaltReasonControlKill, true, cancelCategoryKillSwitch)
	}

	snapshot, err := b.loader.Load(ctx, b.snapshot)
	if err != nil {
		return fmt.Errorf("load startup state: %w", err)
	}
	quotes, err := strategy.BuildQuotesWithOverrides(b.cfg, b.spec, snapshot, b.strategyOverrides())
	if err != nil {
		return fmt.Errorf("build startup quotes: %w", err)
	}
	b.lastQuotes = quotes
	b.applyDerivedState(&snapshot, quotes)
	b.logReferenceSourceTransition(b.snapshot, snapshot)
	b.updateReadiness(snapshot, quotes)
	// Honor pause on startup, not only in RunCycle: a restarting paused bot must cancel and idle,
	// never place a fresh ladder. Without this, a crash-looping paused service re-quotes on every
	// boot — pause silently fails as an incident control exactly when it is relied on.
	if b.cfg.OperatorMode == config.ModePause {
		b.snapshot = snapshot
		return b.haltForReason(ctx, control.HaltReasonOperatorPause, true, "")
	}
	if b.control.Status().Paused {
		b.snapshot = snapshot
		return b.haltForReason(ctx, control.HaltReasonControlPause, true, "")
	}
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
	startupOrders := make(map[string]exchange.Order, len(snapshot.OpenOrders))
	for _, order := range snapshot.OpenOrders {
		startupOrders[order.ID] = order
	}
	for _, id := range result.CanceledOrderIDs {
		order := startupOrders[id]
		b.syncer.recordAction("cancel", order.Side, order.Price, order.Size, id, "startup_reconciliation:"+result.RejectedReasons[id], "")
		b.metrics.IncCancels()
		b.metrics.IncCancelCategory(cancelCategoryStartupReconcile)
		// Same reason as every other cancel: an order we took off the book must not be read as a
		// fill when it stops appearing. ReconcileStartup cancels through the client directly
		// rather than through the syncer, so without this the first comparison after a restart
		// counts the whole previous ladder as fills -- six or seven phantom fills on every boot.
		b.syncer.noteCancelled(id)
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

func marketMetadataAttrs(spec exchange.MarketSpec) []any {
	return []any{
		"market", spec.Symbol,
		"base_asset", spec.BaseAsset,
		"quote_asset", spec.QuoteAsset,
		"asset_address", spec.AssetAddress,
		"quote_address", spec.QuoteAddress,
		"sub_id", spec.SubID,
		"tick_size", spec.TickSize,
		"size_step", spec.SizeStep,
		"min_size", spec.MinSize,
		"order_entry_spec", spec.OrderEntrySpec,
	}
}

// syncFeeSchedule re-reads the market's fee schedule and, when the taker fee has RISEN, cancels
// the resting ladder so it is re-signed against the new number.
//
// Newly placed orders are safe without this -- PlaceLimitOrder derives worstFee from the schedule
// at signing time. The exposure is orders already on the book: reconcileSide keeps an order whose
// price has not drifted, so on a quiet market a quote signed under the old, lower bound can rest
// untouched for its whole lifetime and revert TM_FeeTooHigh on every cross.
//
// Only an increase forces the cancel. A cut leaves every resting bound comfortably above the new
// charge, and churning the book for it would spend cancel budget to no purpose.
//
// The worstFeeHeadroomBps cushion already absorbs a small rise, so this is belt and braces for the
// case that outruns it -- which is the case nobody would notice until fills started reverting.
func (b *Bot) syncFeeSchedule(ctx context.Context) error {
	spec, err := b.client.GetMarket(ctx, b.spec.Symbol)
	if err != nil {
		// GetMarket already falls back to the last good schedule; a real error here means it never
		// had one, and quoting against a schedule we have never seen is not something to guess at.
		return fmt.Errorf("refresh fee schedule: %w", err)
	}

	was := b.spec.TakerFeeBps
	if spec.TakerFeeBps == was {
		return nil
	}
	b.spec = spec

	if spec.TakerFeeBps < was {
		b.logger.Info(
			"taker fee lowered",
			"market", spec.Symbol, "from_bps", was, "to_bps", spec.TakerFeeBps,
			"action", "resting orders kept -- their signed bound still covers the smaller charge",
		)
		return nil
	}

	b.logger.Warn(
		"taker fee raised",
		"market", spec.Symbol, "from_bps", was, "to_bps", spec.TakerFeeBps,
		"action", "cancelling the resting ladder so it is re-signed against the new schedule",
	)
	return b.syncer.CancelAll(ctx, spec.Symbol, cancelCategoryFeeRaised)
}

func (b *Bot) RunCycle(ctx context.Context) (err error) {
	defer func() { b.finishCycle(err) }()
	if active, err := b.killSwitchActive(); err != nil {
		return err
	} else if active {
		if b.killFileSince.IsZero() {
			b.killFileSince = time.Now().UTC()
		}
		return b.haltForReason(ctx, control.HaltReasonKillSwitchFile, true, cancelCategoryKillSwitch)
	}
	b.killFileSince = time.Time{}
	// The control API's kill, on the same footing as the file: before the fee refresh and the load,
	// so a killed bot keeps cancelling whatever it finds even while the exchange is unreachable. The
	// handler has already cancelled once; this pass catches anything a mid-flight cycle placed.
	if b.control.Status().Killed {
		return b.haltForReason(ctx, control.HaltReasonControlKill, true, cancelCategoryKillSwitch)
	}

	if err := b.syncFeeSchedule(ctx); err != nil {
		b.metrics.IncErrors()
		return err
	}

	snapshot, err := b.loader.Load(ctx, b.snapshot)
	if err != nil {
		b.metrics.IncErrors()
		return b.handleLoadError(ctx, err)
	}
	// Drained here, not inside observeFills, so the set covers exactly the interval between the
	// two snapshots being compared -- every cancel this bot issued since the last observation.
	b.observeFills(b.snapshot, snapshot, b.syncer.TakeCancelled())
	quotes, err := strategy.BuildQuotesWithOverrides(b.cfg, b.spec, snapshot, b.strategyOverrides())
	if err != nil {
		b.metrics.IncErrors()
		return err
	}
	b.lastQuotes = quotes
	b.applyDerivedState(&snapshot, quotes)
	b.logReferenceSourceTransition(b.snapshot, snapshot)
	b.updateReadiness(snapshot, quotes)

	riskDecision := risk.Evaluate(b.cfg, b.spec, snapshot)
	if riskDecision.Halt {
		b.snapshot = snapshot
		return b.haltForReason(ctx, riskDecision.Reason, true, cancelCategoryRiskTriggered)
	}
	b.clearDependencyStaleMetrics()
	// Pause is checked before the halt is cleared, not after. Clearing first and re-halting made every
	// paused cycle look like a fresh transition into the halt -- a "halted" warning per poll, and a
	// since-timestamp that reset every two seconds.
	if b.cfg.OperatorMode == config.ModePause {
		b.snapshot = snapshot
		return b.haltForReason(ctx, control.HaltReasonOperatorPause, true, "")
	}
	if b.control.Status().Paused {
		b.snapshot = snapshot
		return b.haltForReason(ctx, control.HaltReasonControlPause, true, "")
	}
	b.metrics.SetHaltState(false, "")
	b.metrics.SetHealth(true, "")
	b.markRunning()

	switch b.cfg.OperatorMode {
	case config.ModeDryRunHealth:
		b.logger.Info("operator mode active", "mode", b.cfg.OperatorMode, "action", "observe_only")
		b.recordInventory(snapshot)
		if err := b.savePersistent(); err != nil {
			return err
		}
		b.snapshot = snapshot
		return nil
	}

	// The refresh throttle is skipped in two cases where waiting is wrong. An operator change (adjust,
	// a restored side, a resume) should reach the book on the cycle it woke, not a refresh interval
	// later. And a resting quote inside its expiry margin must be re-signed now: the throttle defers by
	// up to MM_QUOTE_REFRESH_INTERVAL_MS, and a margin shorter than that would let the quote lapse.
	controlChanged := b.control.TakeDirty()
	expiring := anyExpiring(snapshot.OpenOrders, b.cfg.ExpiryReplaceMarginSeconds, time.Now().UTC())
	if !controlChanged && !expiring && time.Since(snapshot.LastQuoteUpdate) < b.cfg.QuoteRefreshInterval && len(snapshot.OpenOrders) > 0 {
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
	b.logQuoteSuppression(quotes.BidSuppression)
	b.logQuoteSuppression(quotes.AskSuppression)
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

func (b *Bot) allocateIdentities() (map[exchange.Side][]Identity, error) {
	levels := b.cfg.QuoteLevels
	if levels < 1 {
		levels = 1
	}
	base := b.persisted.NextNonceBase
	nowBase := uint64(time.Now().UnixMicro()) * 2
	if nowBase > base {
		base = nowBase
	}
	if base%2 != 0 {
		base++
	}
	// Interleave nonces per level: buy levels get even offsets, sell levels odd,
	// so every order across both sides and all levels has a unique nonce.
	bids := make([]Identity, levels)
	asks := make([]Identity, levels)
	for k := 0; k < levels; k++ {
		bidNonce := base + uint64(2*k)
		askNonce := base + uint64(2*k) + 1
		bids[k] = buildIdentity(b.spec.Symbol, exchange.SideBuy, bidNonce)
		asks[k] = buildIdentity(b.spec.Symbol, exchange.SideSell, askNonce)
	}
	ids := map[exchange.Side][]Identity{
		exchange.SideBuy:  bids,
		exchange.SideSell: asks,
	}
	b.persisted.NextNonceBase = base + uint64(2*levels)
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

func (b *Bot) logQuoteSuppression(s *strategy.Suppression) {
	if s == nil {
		return
	}
	b.logger.Warn("quote side suppressed",
		"side", s.Side,
		"reason", s.Reason,
		"market", s.Market,
		"subaccount_id", s.SubaccountID,
		"anchor_price", s.AnchorPrice,
		"reference_price", s.ReferencePrice,
		"reference_source", s.ReferenceSource,
		"required_capacity", s.RequiredCapacity,
		"total_capacity", s.TotalCapacity,
		"reserved_capacity", s.ReservedCapacity,
		"available_capacity", s.AvailableCapacity,
		"configured_order_size", s.ConfiguredOrderSize,
		"effective_order_size", s.EffectiveOrderSize,
		"min_order_size", s.MinOrderSize,
		"candidate_size", s.CandidateSize,
		"size_step", s.SizeStep,
		"inventory", s.Inventory,
		"max_inventory", s.MaxInventory,
		"spot_asset_address", s.SpotAssetAddress,
		"quote_asset_address", s.QuoteAssetAddress,
		"base_asset", s.BaseAsset,
		"quote_asset", s.QuoteAsset,
		"dependency_stale", s.DependencyStale,
		"external_anchor_failed", s.ExternalAnchorFailed,
		"dry_run", s.DryRun,
		"operator_mode", s.OperatorMode,
	)
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
	b.markRunning()
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
	if reason != b.persisted.LastHaltReason || !b.currentHalted {
		b.haltSince = time.Now().UTC()
		// Log on the transition into a halt (or when the reason changes), so an operator can see
		// why the bot stopped quoting instead of only that it did. The prices show which input was
		// missing — a spot bot with ref==0 and ext_anchor==0 is halted on "reference price
		// unavailable" because neither the book nor the oracle yielded a price.
		b.logger.Warn(
			"halted",
			"reason", reason,
			"reference_price", b.snapshot.ReferencePrice,
			"local_ref", b.snapshot.LocalReferencePrice,
			"ext_anchor", b.snapshot.ExternalAnchorPrice,
		)
	}
	b.persisted.LastHaltReason = reason
	b.haltCount++
	b.currentHalted = true
	b.recordInventory(b.snapshot)
	b.metrics.SetHaltState(true, reason)
	b.metrics.SetHealth(false, reason)
	b.metrics.SetReadiness(false, reason)
	// A halted bot quotes nothing, so the last cycle's targets are not what it would place.
	b.lastQuotes = strategy.Result{}
	if cancelOrders {
		_, remaining, listErr, cancelErr := b.syncer.cancelAll(ctx, b.marketSymbol, cancelCategory, "")
		switch {
		case listErr == nil:
			// What the cancel-all left resting is the book as of now. A kill returns before the
			// cycle's load, so without this /control/state would show the pre-kill ladder for as long
			// as the bot stayed killed -- to an operator checking that the kill emptied the book.
			b.snapshot.OpenOrders = remaining
		case !b.cfg.DryRun:
			return listErr
		}
		if cancelErr != nil {
			return cancelErr
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
		// A side the operator pulled is not "missing"; failing readiness over it would page for an
		// instruction that was followed.
		sides := b.control.Status()
		requiredBid := (b.cfg.OperatorMode == config.ModeNormal || b.cfg.OperatorMode == config.ModeBidOnly) && sides.BidEnabled
		requiredAsk := (b.cfg.OperatorMode == config.ModeNormal || b.cfg.OperatorMode == config.ModeAskOnly) && sides.AskEnabled
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
		"state=%s halted=%t inv=%0.6f bids=%d asks=%d fills_buy=%d fills_sell=%d partial_fills=%d cancels=%d md_age=%s bal_age=%s anchor_age=%s halt_reason=%q ref=%0.6f local_ref=%0.6f ext_anchor=%0.6f best_bid=%0.6f best_ask=%0.6f",
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
		s.LastHaltReason,
		b.snapshot.ReferencePrice,
		b.snapshot.LocalReferencePrice,
		b.snapshot.ExternalAnchorPrice,
		b.snapshot.BestBid,
		b.snapshot.BestAsk,
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

func (b *Bot) observeFills(previous, current state.Snapshot, cancelled map[string]struct{}) {
	if fillsBySide, partials, ok := observeOrderStateFills(previous, current, cancelled); ok {
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
	// A balance change on its own is NOT evidence of a trade, and the inventory-delta fallback that
	// used to live here could not tell the two apart. On 2026-09-16 a 10 USDC deposit into
	// subaccount 15 arrived while five bids rested untouched and the venue's tape was unchanged, so
	// both observations above abstained and the delta was booked as a buy fill that never happened.
	// Deposits, withdrawals and transfers all move inventory without trading.
	//
	// Every real fill leaves one of two marks: it changes one of our resting orders, or it appears
	// on the venue's tape. Anything left over is somebody moving money, so nothing is counted.
}

// observeOrderStateFills turns two consecutive snapshots into fill counts.
//
// An order present before and absent now has either been filled or been cancelled, and the two are
// indistinguishable from the snapshots alone -- so the caller supplies the set of orders this bot
// cancelled in that interval. Without it the bot counted its own churn: 9,066 reported fills in 15
// hours against a venue with 8 trades in its entire history, which both misreported the fill rate
// an operator reads and disguised the churn bug that was generating the cancels.
//
// A disappearance that is neither a fill nor one of our cancels -- an expiry, or a cancel from
// somewhere else -- is still counted as a fill here. That remains wrong, but it is rare and
// bounded, where counting every replace was neither.
func observeOrderStateFills(previous, current state.Snapshot, cancelled map[string]struct{}) (map[string]uint64, uint64, bool) {
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
			if _, wasCancelled := cancelled[id]; wasCancelled {
				// We took it off the book. Its absence says nothing about trading.
				haveTruth = true
				continue
			}
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
