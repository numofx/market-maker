package execution

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
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

	// cancelledIDs is every order this bot cancelled since the caller last drained it.
	//
	// It exists so fills can be told apart from cancels. observeOrderStateFills sees an order
	// that was on the previous snapshot and not on the current one, and without this it counts
	// that as a fill -- but the bot's own cancels make orders disappear exactly the same way, so
	// it was counting its own churn. Over one 15-hour window that produced 9,066 reported fills
	// against a venue whose entire trade history was 8 trades.
	cancelledIDs map[string]struct{}
}

const (
	cancelCategoryReplaceDriven    = "replace_driven"
	cancelCategoryStartupReconcile = "startup_reconciliation"
	cancelCategoryRiskTriggered    = "risk_triggered"
	cancelCategoryKillSwitch       = "kill_switch"
	cancelCategoryFeeRaised        = "taker_fee_raised"
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
			quantum := sizeQuantumUI(s.spec, current.Price)
			decision := evaluateCancel(current, target, opposite, s.cfg, snapshot.LastQuoteUpdate, time.Now().UTC(), quantum)
			switch {
			case decision.Suppress:
				s.logger.Info("replace suppressed", "order_id", current.ID, "side", current.Side, "reason", decision.SuppressReason)
				s.metrics.IncSuppressedReplaces()
			case decision.Cancel && decision.EnforceRateLimit && !s.canUseCancelSlot():
				// Out of cancel budget. Leave the order resting and try again next cycle.
				//
				// This used to return CancelRateLimitError, which the bot answered by halting and
				// cancelling the ENTIRE ladder -- through CancelAll, which passes recordRate=false
				// and so bypasses the very limiter that just tripped. The response to "you are
				// cancelling too much" was to cancel everything, go dark, and rebuild all six
				// orders next cycle, which costs six more placements and starts the loop again.
				//
				// A slightly stale quote is better than no quote. The budget is there to bound
				// churn, and declining the replace bounds it; emptying the book does not.
				s.logger.Info(
					"replace suppressed", "order_id", current.ID, "side", current.Side,
					"reason", "cancel budget exhausted", "limit_per_minute", s.cfg.MaxCancelsPerMinute,
				)
				s.metrics.IncSuppressedReplaces()
			case decision.Cancel:
				if decision.Reason == "size_mismatch" {
					s.logger.Info("size mismatch replace", sizeMismatchAttrs(current, target, quantum, s.cfg)...)
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

func evaluateCancel(current *exchange.Order, target *strategy.Quote, opposite *strategy.Quote, cfg config.Config, fallbackQuoteTime time.Time, now time.Time, quantum float64) cancelDecision {
	if current == nil {
		return cancelDecision{}
	}
	if target == nil {
		return cancelDecision{Cancel: true, Reason: "no_target"}
	}
	if current.Side != target.Side {
		return cancelDecision{Cancel: true, Reason: "side_mismatch"}
	}
	if sizeMismatchRequiresReplace(current.Size, target.Size, cfg, quantum) {
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

// sizeQuantumUI is the smallest size difference the venue can actually express, in the same units
// the quote is denominated in.
//
// Spot amounts are whole cNGN, so placing a UI size of 0.359975 USDC at 1331.99 asks for 479.48
// cNGN and rests 479 -- the venue floors it, and the resting order is smaller than the target by
// up to one cNGN no matter what anyone does. In UI terms that is 1/price USDC.
//
// Futures quantise in contract lots, where the size is already in contracts and the step is
// MinSize.
// sizeMismatchAttrs is every input the replace decision actually used, so a size_mismatch cancel
// can be explained from the log line alone.
//
// It exists because it could not be. On 2026-09-11 a quantum floor was added to stop the ladder
// replacing itself over differences the venue cannot express, and it cut the churn by 16% rather
// than the expected ~92%. Running the same function on the numbers read off /v1/book and the
// place-order logs said "keep"; production said "replace". The decision inputs were never logged,
// so the gap could only be guessed at -- twice, wrongly.
//
// raw_engine_amount is the value straight from the active_orders row, before
// orderAmountToFloat and spotUIFromEngine convert it. The book serves the same order through a
// different projection, and the suspicion is that the two disagree; printing both ends of that
// conversion is what settles it rather than another inference.
func sizeMismatchAttrs(current *exchange.Order, target *strategy.Quote, quantum float64, cfg config.Config) []any {
	relative := math.Max(math.Abs(current.Size), math.Abs(target.Size)) * (sizeDustToleranceBPS / 10000.0)
	tolerance := math.Max(math.Max(cfg.AdoptSizeTolerance, relative), quantum)

	attrs := []any{
		"order_id", current.ID,
		"side", current.Side,
		"current_size", current.Size,
		"target_size", target.Size,
		"diff", math.Abs(current.Size - target.Size),
		"quantum", quantum,
		"tolerance", tolerance,
		"tolerance_source", toleranceSource(cfg.AdoptSizeTolerance, relative, quantum),
		"current_price", current.Price,
		"target_price", target.Price,
		"raw_engine_amount", current.RawSize,
	}
	// What current.Size would be if it were derived from the raw amount and the order's own price.
	// A disagreement with current_size localises the fault to the conversion rather than the
	// decision.
	if raw, err := strconv.ParseFloat(strings.TrimSpace(current.RawSize), 64); err == nil && current.Price > 0 {
		attrs = append(attrs, "size_from_raw", raw/current.Price)
	}
	return attrs
}

// toleranceSource names which of the three bounds actually won, so a line that replaces despite
// the quantum floor shows immediately whether the floor was in play at all.
func toleranceSource(absolute, relative, quantum float64) string {
	switch {
	case quantum >= absolute && quantum >= relative:
		return "quantum"
	case relative >= absolute:
		return "relative_dust_bps"
	default:
		return "absolute_adopt_tolerance"
	}
}

func sizeQuantumUI(spec exchange.MarketSpec, price float64) float64 {
	if spec.Symbol == "USDCcNGN-SPOT" {
		if price <= 0 {
			return 0
		}
		return 1.0 / price
	}
	return spec.MinSize
}

// sizeMismatchRequiresReplace reports whether a resting order's size is far enough from the target
// to be worth cancelling and replacing.
//
// The tolerance is floored at one quantum because the venue cannot express anything finer. The
// dust tolerance is RELATIVE (5 bps of size) while the quantisation error is ABSOLUTE (up to one
// cNGN), so below roughly 1.5 USDC the error always exceeds the tolerance and the order is
// replaced by an order that is wrong in exactly the same way -- forever, at the poll interval.
//
// That is not hypothetical. On 2026-09-10 a 0.359975 USDC level was placed 4,065 times at an
// unchanged price: target 0.359975, rests 0.359613, diff 0.000362, tolerance 0.000180. It
// accounted for 92% of 10,368 cancels in 15 hours and repeatedly exhausted the cancel budget.
// There were no fills to explain it -- the venue's whole trade history was 8 trades.
func sizeMismatchRequiresReplace(current, target float64, cfg config.Config, quantum float64) bool {
	diff := math.Abs(current - target)
	if diff <= 1e-9 {
		return false
	}
	absTolerance := cfg.AdoptSizeTolerance
	relativeTolerance := math.Max(math.Abs(current), math.Abs(target)) * (sizeDustToleranceBPS / 10000.0)
	return diff > math.Max(math.Max(absTolerance, relativeTolerance), quantum)
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
	s.noteCancelled(orderID)
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

// noteCancelled records an order the bot itself took off the book, so its disappearance is not
// later mistaken for a fill. Recorded on intent rather than on success: an order we asked the venue
// to cancel is not evidence of a trade whether or not the call returned cleanly.
func (s *Syncer) noteCancelled(orderID string) {
	if s.cancelledIDs == nil {
		s.cancelledIDs = make(map[string]struct{})
	}
	s.cancelledIDs[orderID] = struct{}{}
}

// TakeCancelled returns the orders cancelled since the last call and clears the set.
//
// Draining is deliberate. The caller compares two consecutive snapshots, so the only cancels that
// can explain a disappearance are the ones issued between them; holding older ids would suppress a
// genuine fill on an order id the venue happened to reuse.
func (s *Syncer) TakeCancelled() map[string]struct{} {
	out := s.cancelledIDs
	s.cancelledIDs = nil
	if out == nil {
		return map[string]struct{}{}
	}
	return out
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

// shouldCancel is the price-drift-only helper used by the unit tests. Quantum 0 keeps it on the
// old tolerance arithmetic, which is what those cases are about.
func shouldCancel(current *exchange.Order, target *strategy.Quote, staleThresholdBPS float64, opposite *strategy.Quote) bool {
	decision := evaluateCancel(current, target, opposite, config.Config{CancelStaleOrderThreshold: staleThresholdBPS}, time.Time{}, time.Now().UTC(), 0)
	return decision.Cancel
}
