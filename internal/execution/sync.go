package execution

import (
	"context"
	"fmt"
	"log/slog"
	"math"
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
	if err := s.client.CancelAllOrders(ctx, market); err != nil {
		s.metrics.IncErrors()
		return err
	}
	for range orders {
		s.metrics.IncCancels()
		if category != "" {
			s.metrics.IncCancelCategory(category)
		}
		s.recordCancel()
	}
	return nil
}

func (s *Syncer) Sync(ctx context.Context, snapshot state.Snapshot, quotes strategy.Result, identities map[exchange.Side]Identity) (SyncResult, error) {
	var (
		existingBid *exchange.Order
		existingAsk *exchange.Order
		result      = SyncResult{PlacedOrderIDs: make(map[exchange.Side]string)}
	)

	for i := range snapshot.OpenOrders {
		order := snapshot.OpenOrders[i]
		switch order.Side {
		case exchange.SideBuy:
			if existingBid == nil {
				existingBid = &order
				continue
			}
		case exchange.SideSell:
			if existingAsk == nil {
				existingAsk = &order
				continue
			}
		}
		if err := s.cancel(ctx, order.ID, "duplicate_side", false, ""); err != nil {
			return result, err
		}
		result.Changed = true
	}

	targets := []struct {
		side     exchange.Side
		current  **exchange.Order
		target   *strategy.Quote
		opposite *strategy.Quote
	}{
		{side: exchange.SideBuy, current: &existingBid, target: quotes.Bid, opposite: quotes.Ask},
		{side: exchange.SideSell, current: &existingAsk, target: quotes.Ask, opposite: quotes.Bid},
	}

	for _, item := range targets {
		if *item.current != nil {
			decision := evaluateCancel(*item.current, item.target, item.opposite, s.cfg, snapshot.LastQuoteUpdate, time.Now().UTC())
			switch {
			case decision.Suppress:
				s.logger.Info("replace suppressed", "order_id", (*item.current).ID, "side", (*item.current).Side, "reason", decision.SuppressReason)
				s.metrics.IncSuppressedReplaces()
			case decision.Cancel:
				if decision.EnforceRateLimit && !s.canUseCancelSlot() {
					return result, &CancelRateLimitError{Limit: s.cfg.MaxCancelsPerMinute}
				}
				if err := s.cancel(ctx, (*item.current).ID, decision.Reason, decision.EnforceRateLimit, cancelCategoryReplaceDriven); err != nil {
					return result, err
				}
				result.Changed = true
				*item.current = nil
			}
		}
		if *item.current == nil && item.target != nil {
			id := identities[item.side]
			if err := s.place(ctx, snapshot.Market, *item.target, id); err != nil {
				return result, err
			}
			result.Changed = true
			result.PlacedOrderIDs[item.side] = id.OrderID
		}
	}

	s.metrics.SetOpenBidPresent(quotes.Bid != nil || existingBid != nil)
	s.metrics.SetOpenAskPresent(quotes.Ask != nil || existingAsk != nil)
	s.metrics.SetCancelsPerMinute(s.cancelsPerMinute())
	if result.Changed {
		s.metrics.IncQuoteRefresh()
	}
	return result, nil
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
	if math.Abs(current.Size-target.Size) > 1e-9 {
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

func priceDriftBPS(current, target float64) float64 {
	if current <= 0 || target <= 0 {
		return math.Inf(1)
	}
	return math.Abs(target-current) / current * 10000.0
}

func (s *Syncer) cancel(ctx context.Context, orderID string, reason string, recordRate bool, category string) error {
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
	if err := s.client.CancelOrder(ctx, orderID); err != nil {
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
