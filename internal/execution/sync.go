package execution

import (
	"context"
	"fmt"
	"log/slog"
	"math"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/metrics"
	"github.com/numofx/market-maker/internal/state"
	"github.com/numofx/market-maker/internal/strategy"
)

type Syncer struct {
	client  exchange.Client
	spec    exchange.MarketSpec
	cfg     config.Config
	metrics *metrics.Registry
	logger  *slog.Logger
}

func NewSyncer(client exchange.Client, spec exchange.MarketSpec, cfg config.Config, m *metrics.Registry, logger *slog.Logger) *Syncer {
	return &Syncer{client: client, spec: spec, cfg: cfg, metrics: m, logger: logger}
}

func (s *Syncer) CancelAll(ctx context.Context, market string) error {
	if s.cfg.DryRun {
		s.logger.Info("dry-run cancel-all", "market", market)
		return nil
	}
	if err := s.client.CancelAllOrders(ctx, market); err != nil {
		s.metrics.IncErrors()
		return err
	}
	s.metrics.IncCancels()
	return nil
}

func (s *Syncer) Sync(ctx context.Context, snapshot state.Snapshot, quotes strategy.Result, identities map[exchange.Side]Identity) (bool, error) {
	var (
		existingBid *exchange.Order
		existingAsk *exchange.Order
		changed     bool
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
		if err := s.cancel(ctx, order.ID, "duplicate_side"); err != nil {
			return changed, err
		}
		changed = true
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
		if *item.current != nil && shouldCancel(*item.current, item.target, s.cfg.CancelStaleOrderThreshold, item.opposite) {
			if err := s.cancel(ctx, (*item.current).ID, "stale_or_wrong"); err != nil {
				return changed, err
			}
			changed = true
			*item.current = nil
		}
		if *item.current == nil && item.target != nil {
			id := identities[item.side]
			if err := s.place(ctx, snapshot.Market, *item.target, id); err != nil {
				return changed, err
			}
			changed = true
		}
	}

	s.metrics.SetOpenBidPresent(quotes.Bid != nil || existingBid != nil)
	s.metrics.SetOpenAskPresent(quotes.Ask != nil || existingAsk != nil)
	if changed {
		s.metrics.IncQuoteRefresh()
	}
	return changed, nil
}

func shouldCancel(current *exchange.Order, target *strategy.Quote, staleThresholdBPS float64, opposite *strategy.Quote) bool {
	if current == nil {
		return false
	}
	if target == nil {
		return true
	}
	if current.Side != target.Side {
		return true
	}
	if math.Abs(current.Size-target.Size) > 1e-9 {
		return true
	}
	if priceDriftBPS(current.Price, target.Price) >= staleThresholdBPS {
		return true
	}
	if opposite != nil {
		if current.Side == exchange.SideBuy && target.Price >= opposite.Price {
			return true
		}
		if current.Side == exchange.SideSell && opposite.Price >= target.Price {
			return true
		}
	}
	return false
}

func priceDriftBPS(current, target float64) float64 {
	if current <= 0 || target <= 0 {
		return math.Inf(1)
	}
	return math.Abs(target-current) / current * 10000.0
}

func (s *Syncer) cancel(ctx context.Context, orderID string, reason string) error {
	s.logger.Info("cancel order", "order_id", orderID, "reason", reason)
	if s.cfg.DryRun {
		return nil
	}
	if err := s.client.CancelOrder(ctx, orderID); err != nil {
		s.metrics.IncErrors()
		return fmt.Errorf("cancel order %s: %w", orderID, err)
	}
	s.metrics.IncCancels()
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
	return nil
}
