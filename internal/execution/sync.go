package execution

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/metrics"
	"github.com/numofx/market-maker/internal/state"
	"github.com/numofx/market-maker/internal/strategy"
)

type Syncer struct {
	client           exchange.Client
	spec             exchange.MarketSpec
	cfg              config.Config
	metrics          *metrics.Registry
	logger           *slog.Logger
	cancelTimestamps []time.Time
	totalCancels     uint64
	totalPlacements  uint64
}

const (
	cancelCategoryReplaceDriven    = "replace_driven"
	cancelCategoryStartupReconcile = "startup_reconciliation"
	cancelCategoryRiskTriggered    = "risk_triggered"
	cancelCategoryKillSwitch       = "kill_switch"
	sizeDustToleranceBPS           = 5.0
)

type SyncResult struct {
	Changed        bool
	PlacedOrderIDs map[exchange.Side]string
}

type CancelRateLimitError struct {
	Limit int
}

func (e *CancelRateLimitError) Error() string {
	return fmt.Sprintf("cancel rate limit exceeded: max %d per minute", e.Limit)
}

func NewSyncer(client exchange.Client, spec exchange.MarketSpec, cfg config.Config, m *metrics.Registry, logger *slog.Logger) *Syncer {
	return &Syncer{client: client, spec: spec, cfg: cfg, metrics: m, logger: logger}
}

func (s *Syncer) CancelAll(ctx context.Context, market string, category string) error {
	if s.cfg.DryRun {
		s.logger.Info("dry-run cancel-all", "market", market)
		return nil
	}
	orders, err := s.client.ListOpenOrders(ctx, market)
	if err != nil {
		s.metrics.IncErrors()
		return err
	}
	for _, order := range orders {
		if err := s.cancel(ctx, order.ID, "cancel_all", false, category); err != nil {
			if strings.Contains(err.Error(), "active order not found") {
				continue
			}
			s.metrics.IncErrors()
			return err
		}
	}
	return nil
}

func (s *Syncer) Sync(ctx context.Context, snapshot state.Snapshot, quotes strategy.Result, identities map[exchange.Side][]Identity) (SyncResult, error) {
	result := SyncResult{PlacedOrderIDs: make(map[exchange.Side]string)}

	// A ladder is the source of truth; fall back to the single Bid/Ask when only
	// the best level was set (older callers / one-level results).
	bidTargets := quotes.Bids
	if len(bidTargets) == 0 && quotes.Bid != nil {
		bidTargets = []strategy.Quote{*quotes.Bid}
	}
	askTargets := quotes.Asks
	if len(askTargets) == 0 && quotes.Ask != nil {
		askTargets = []strategy.Quote{*quotes.Ask}
	}

	// Group resting orders by side, best price first (bids descending, asks
	// ascending), so they pair against the target ladder by price rank.
	existingBids, existingAsks := groupOrdersBySide(snapshot.OpenOrders)

	// The cross-quote guard compares each level against the BEST opposite quote
	// (top of book), which is the only one it could realistically cross.
	bestBid := firstQuote(bidTargets)
	bestAsk := firstQuote(askTargets)

	if err := s.reconcileSide(ctx, snapshot, existingBids, bidTargets, bestAsk, identities[exchange.SideBuy], &result); err != nil {
		return result, err
	}
	if err := s.reconcileSide(ctx, snapshot, existingAsks, askTargets, bestBid, identities[exchange.SideSell], &result); err != nil {
		return result, err
	}

	s.metrics.SetOpenBidPresent(len(bidTargets) > 0 || len(existingBids) > 0)
	s.metrics.SetOpenAskPresent(len(askTargets) > 0 || len(existingAsks) > 0)
	s.metrics.SetCancelsPerMinute(s.cancelsPerMinute())
	if result.Changed {
		s.metrics.IncQuoteRefresh()
	}
	return result, nil
}

// reconcileSide pairs the resting orders of one side (best-first) against the
// target ladder (best-first) by rank: slot k's resting order is replaced/kept
// against target level k, resting orders beyond the ladder depth are cancelled,
// and target levels beyond the resting depth are placed. With one target and one
// resting order this is exactly the original single-level reconcile.
func (s *Syncer) reconcileSide(
	ctx context.Context,
	snapshot state.Snapshot,
	existing []exchange.Order,
	targets []strategy.Quote,
	opposite *strategy.Quote,
	ids []Identity,
	result *SyncResult,
) error {
	slots := len(existing)
	if len(targets) > slots {
		slots = len(targets)
	}
	for i := 0; i < slots; i++ {
		var current *exchange.Order
		if i < len(existing) {
			current = &existing[i]
		}
		var target *strategy.Quote
		if i < len(targets) {
			target = &targets[i]
		}

		if current != nil {
			decision := evaluateCancel(current, target, opposite, s.cfg, snapshot.LastQuoteUpdate, time.Now().UTC())
			switch {
			case decision.Suppress:
				s.logger.Info("replace suppressed", "order_id", current.ID, "side", current.Side, "reason", decision.SuppressReason)
				s.metrics.IncSuppressedReplaces()
			case decision.Cancel:
				if decision.EnforceRateLimit && !s.canUseCancelSlot() {
					return &CancelRateLimitError{Limit: s.cfg.MaxCancelsPerMinute}
				}
				if err := s.cancel(ctx, current.ID, decision.Reason, decision.EnforceRateLimit, cancelCategoryReplaceDriven); err != nil {
					return err
				}
				result.Changed = true
				current = nil
			}
		}
		if current == nil && target != nil {
			if i >= len(ids) {
				// No identity allocated for this depth; skip rather than reuse a nonce.
				s.logger.Warn("no identity for quote level", "side", target.Side, "level", i)
				continue
			}
			id := ids[i]
			if err := s.place(ctx, snapshot.Market, *target, id); err != nil {
				return err
			}
			result.Changed = true
			if _, ok := result.PlacedOrderIDs[target.Side]; !ok {
				result.PlacedOrderIDs[target.Side] = id.OrderID
			}
		}
	}
	return nil
}

// groupOrdersBySide splits resting orders into bids (price descending) and asks
// (price ascending) so each side's best-priced order is first.
func groupOrdersBySide(orders []exchange.Order) ([]exchange.Order, []exchange.Order) {
	var bids, asks []exchange.Order
	for _, order := range orders {
		switch order.Side {
		case exchange.SideBuy:
			bids = append(bids, order)
		case exchange.SideSell:
			asks = append(asks, order)
		}
	}
	sort.SliceStable(bids, func(i, j int) bool { return bids[i].Price > bids[j].Price })
	sort.SliceStable(asks, func(i, j int) bool { return asks[i].Price < asks[j].Price })
	return bids, asks
}

func firstQuote(quotes []strategy.Quote) *strategy.Quote {
	if len(quotes) == 0 {
		return nil
	}
	return &quotes[0]
}

type cancelDecision struct {
	Cancel           bool
	Reason           string
	EnforceRateLimit bool
	Suppress         bool
	SuppressReason   string
}

func evaluateCancel(current *exchange.Order, target *strategy.Quote, opposite *strategy.Quote, cfg config.Config, fallbackQuoteTime time.Time, now time.Time) cancelDecision {
	if current == nil {
		return cancelDecision{}
	}
	if target == nil {
		return cancelDecision{Cancel: true, Reason: "no_target"}
	}
	if current.Side != target.Side {
		return cancelDecision{Cancel: true, Reason: "side_mismatch"}
	}
	if sizeMismatchRequiresReplace(current.Size, target.Size, cfg) {
		return cancelDecision{Cancel: true, Reason: "size_mismatch", EnforceRateLimit: true}
	}
	drift := priceDriftBPS(current.Price, target.Price)
	if drift >= cfg.CancelStaleOrderThreshold {
		orderAge := quoteAge(current, fallbackQuoteTime, now)
		if cfg.MinQuoteLifetime > 0 && orderAge < cfg.MinQuoteLifetime {
			return cancelDecision{Suppress: true, SuppressReason: "minimum_lifetime_not_met"}
		}
		if cfg.MinReplaceMoveBPS > 0 && drift < cfg.MinReplaceMoveBPS {
			return cancelDecision{Suppress: true, SuppressReason: "minimum_move_not_met"}
		}
		return cancelDecision{Cancel: true, Reason: "stale_or_wrong", EnforceRateLimit: true}
	}
	if opposite != nil {
		if current.Side == exchange.SideBuy && target.Price >= opposite.Price {
			return cancelDecision{Cancel: true, Reason: "crossing_own_quotes"}
		}
		if current.Side == exchange.SideSell && opposite.Price >= target.Price {
			return cancelDecision{Cancel: true, Reason: "crossing_own_quotes"}
		}
	}
	return cancelDecision{}
}

func sizeMismatchRequiresReplace(current, target float64, cfg config.Config) bool {
	diff := math.Abs(current - target)
	if diff <= 1e-9 {
		return false
	}
	absTolerance := cfg.AdoptSizeTolerance
	relativeTolerance := math.Max(math.Abs(current), math.Abs(target)) * (sizeDustToleranceBPS / 10000.0)
	return diff > math.Max(absTolerance, relativeTolerance)
}

func priceDriftBPS(current, target float64) float64 {
	if current <= 0 || target <= 0 {
		return math.Inf(1)
	}
	return math.Abs(target-current) / current * 10000.0
}

func (s *Syncer) cancel(ctx context.Context, orderID string, reason string, recordRate bool, category string) error {
	if isProtectedOrderID(s.cfg, orderID) {
		s.logger.Info("skip protected order cancel", "order_id", orderID, "reason", reason)
		return nil
	}
	s.logger.Info("cancel order", "order_id", orderID, "reason", reason)
	if s.cfg.DryRun {
		if recordRate {
			s.recordCancel()
			s.metrics.SetCancelsPerMinute(s.cancelsPerMinute())
		}
		s.metrics.IncCancels()
		if category != "" {
			s.metrics.IncCancelCategory(category)
		}
		return nil
	}
	if err := s.client.CancelOrder(ctx, orderID, reason); err != nil {
		s.metrics.IncErrors()
		return fmt.Errorf("cancel order %s: %w", orderID, err)
	}
	s.metrics.IncCancels()
	if category != "" {
		s.metrics.IncCancelCategory(category)
	}
	if recordRate {
		s.recordCancel()
		s.metrics.SetCancelsPerMinute(s.cancelsPerMinute())
	}
	return nil
}

func (s *Syncer) place(ctx context.Context, market string, q strategy.Quote, id Identity) error {
	s.logger.Info("place order", "market", market, "side", q.Side, "price", q.Price, "size", q.Size, "order_id", id.OrderID, "nonce", id.Nonce)
	if s.cfg.DryRun {
		return nil
	}
	if _, err := s.client.PlaceLimitOrder(ctx, exchange.PlaceOrderRequest{
		Market:  market,
		Side:    q.Side,
		Price:   q.Price,
		Size:    q.Size,
		OrderID: id.OrderID,
		Nonce:   id.Nonce,
	}); err != nil {
		s.metrics.IncErrors()
		return fmt.Errorf("place order: %w", err)
	}
	s.metrics.IncPlacements()
	s.totalPlacements++
	s.metrics.SetCancelReplaceRatio(s.cancelReplaceRatio())
	return nil
}

func (s *Syncer) canUseCancelSlot() bool {
	if s.cfg.MaxCancelsPerMinute <= 0 {
		return true
	}
	s.pruneCancelTimestamps(time.Now().UTC())
	return len(s.cancelTimestamps) < s.cfg.MaxCancelsPerMinute
}

func (s *Syncer) recordCancel() {
	now := time.Now().UTC()
	s.pruneCancelTimestamps(now)
	s.cancelTimestamps = append(s.cancelTimestamps, now)
	s.totalCancels++
	s.metrics.SetCancelReplaceRatio(s.cancelReplaceRatio())
}

func (s *Syncer) pruneCancelTimestamps(now time.Time) {
	if len(s.cancelTimestamps) == 0 {
		return
	}
	cutoff := now.Add(-time.Minute)
	idx := 0
	for idx < len(s.cancelTimestamps) && s.cancelTimestamps[idx].Before(cutoff) {
		idx++
	}
	if idx > 0 {
		s.cancelTimestamps = append([]time.Time(nil), s.cancelTimestamps[idx:]...)
	}
}

func (s *Syncer) cancelsPerMinute() float64 {
	s.pruneCancelTimestamps(time.Now().UTC())
	return float64(len(s.cancelTimestamps))
}

func (s *Syncer) cancelReplaceRatio() float64 {
	if s.totalPlacements == 0 {
		return 0
	}
	return float64(s.totalCancels) / float64(s.totalPlacements)
}

func quoteAge(current *exchange.Order, fallbackQuoteTime, now time.Time) time.Duration {
	if current != nil && !current.CreatedAt.IsZero() {
		return now.Sub(current.CreatedAt)
	}
	if !fallbackQuoteTime.IsZero() {
		return now.Sub(fallbackQuoteTime)
	}
	return 0
}

func shouldCancel(current *exchange.Order, target *strategy.Quote, staleThresholdBPS float64, opposite *strategy.Quote) bool {
	decision := evaluateCancel(current, target, opposite, config.Config{CancelStaleOrderThreshold: staleThresholdBPS}, time.Time{}, time.Now().UTC())
	return decision.Cancel
}
