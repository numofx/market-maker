package execution

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/risk"
	"github.com/numofx/market-maker/internal/state"
	"github.com/numofx/market-maker/internal/strategy"
)

type ReconciliationResult struct {
	AdoptedBidOrderID string
	AdoptedAskOrderID string
	CanceledOrderIDs  []string
	RejectedReasons   map[string]string
}

func ReconcileStartup(
	ctx context.Context,
	client exchange.Client,
	cfg config.Config,
	spec exchange.MarketSpec,
	snapshot state.Snapshot,
	quotes strategy.Result,
	logger *slog.Logger,
) (ReconciliationResult, []exchange.Order, error) {
	result := ReconciliationResult{
		CanceledOrderIDs: make([]string, 0),
		RejectedReasons:  make(map[string]string),
	}
	orderByID := make(map[string]exchange.Order, len(snapshot.OpenOrders))
	for _, order := range snapshot.OpenOrders {
		orderByID[order.ID] = order
		logger.Info("startup order found", "order_id", order.ID, "side", order.Side, "price", order.Price, "size", order.Size, "managed", order.Managed, "nonce", order.Nonce)
	}

	riskDecision := risk.Evaluate(cfg, spec, snapshot)
	if riskDecision.Halt {
		for _, order := range snapshot.OpenOrders {
			result.RejectedReasons[order.ID] = "risk_halt:" + riskDecision.Reason
			cancelled, err := cancelStartupOrder(ctx, client, cfg, order, logger, "risk_halt")
			if err != nil {
				return result, nil, err
			}
			if cancelled {
				result.CanceledOrderIDs = append(result.CanceledOrderIDs, order.ID)
			}
		}
		sort.Strings(result.CanceledOrderIDs)
		return result, nil, nil
	}

	classified := map[exchange.Side][]exchange.Order{
		exchange.SideBuy:  {},
		exchange.SideSell: {},
	}
	for _, order := range snapshot.OpenOrders {
		reason := startupRejectReason(spec, order)
		if reason != "" {
			result.RejectedReasons[order.ID] = reason
			continue
		}
		classified[order.Side] = append(classified[order.Side], order)
	}

	adopted := make(map[exchange.Side]string)
	for _, side := range []exchange.Side{exchange.SideBuy, exchange.SideSell} {
		target := quoteForSide(quotes, side)
		orders := classified[side]
		if len(orders) == 0 {
			continue
		}
		if len(orders) > 1 {
			for _, order := range orders {
				result.RejectedReasons[order.ID] = "duplicate_side"
			}
			continue
		}
		order := orders[0]
		if reason := adoptionMismatchReason(cfg, order, target); reason != "" {
			result.RejectedReasons[order.ID] = reason
			continue
		}
		adopted[side] = order.ID
		if side == exchange.SideBuy {
			result.AdoptedBidOrderID = order.ID
		} else {
			result.AdoptedAskOrderID = order.ID
		}
	}

	cancelSet := make(map[string]struct{})
	for _, order := range snapshot.OpenOrders {
		if _, ok := adopted[order.Side]; ok && adopted[order.Side] == order.ID {
			continue
		}
		if _, rejected := result.RejectedReasons[order.ID]; rejected {
			cancelSet[order.ID] = struct{}{}
		}
	}

	for id := range cancelSet {
		order := orderByID[id]
		cancelled, err := cancelStartupOrder(ctx, client, cfg, order, logger, result.RejectedReasons[id])
		if err != nil {
			return result, nil, err
		}
		if cancelled {
			result.CanceledOrderIDs = append(result.CanceledOrderIDs, id)
		}
	}
	sort.Strings(result.CanceledOrderIDs)

	adoptedOrders := make([]exchange.Order, 0, 2)
	if result.AdoptedBidOrderID != "" {
		adoptedOrders = append(adoptedOrders, orderByID[result.AdoptedBidOrderID])
	}
	if result.AdoptedAskOrderID != "" {
		adoptedOrders = append(adoptedOrders, orderByID[result.AdoptedAskOrderID])
	}

	logger.Info(
		"startup reconciliation result",
		"adopted_bid", result.AdoptedBidOrderID,
		"adopted_ask", result.AdoptedAskOrderID,
		"canceled_order_ids", result.CanceledOrderIDs,
		"rejected_reasons", result.RejectedReasons,
	)
	return result, adoptedOrders, nil
}

func startupRejectReason(spec exchange.MarketSpec, order exchange.Order) string {
	if !order.Managed {
		return "ambiguous_ownership"
	}
	if !strings.HasPrefix(order.ID, startupManagedOrderPrefix(spec.Symbol)) {
		return "malformed_metadata"
	}
	parts := strings.Split(order.ID, ":")
	if len(parts) != 4 {
		return "malformed_metadata"
	}
	if parts[1] != spec.Symbol {
		return "wrong_market"
	}
	if order.Side != exchange.SideBuy && order.Side != exchange.SideSell {
		return "invalid_side"
	}
	wantSide := string(order.Side)
	if parts[2] != wantSide {
		return "malformed_metadata"
	}
	return ""
}

func startupManagedOrderPrefix(market string) string {
	return "mm:" + market + ":"
}

func adoptionMismatchReason(cfg config.Config, order exchange.Order, target *strategy.Quote) string {
	if target == nil {
		return "strategy_not_quoting_side"
	}
	if priceDriftBPS(order.Price, target.Price) >= cfg.CancelStaleOrderThreshold {
		return "price_too_far_from_target"
	}
	if math.Abs(order.Size-target.Size) > cfg.AdoptSizeTolerance {
		return "size_too_far_from_target"
	}
	return ""
}

func quoteForSide(result strategy.Result, side exchange.Side) *strategy.Quote {
	if side == exchange.SideBuy {
		return result.Bid
	}
	return result.Ask
}

func cancelStartupOrder(ctx context.Context, client exchange.Client, cfg config.Config, order exchange.Order, logger *slog.Logger, reason string) (bool, error) {
	if isProtectedOrderID(cfg, order.ID) {
		logger.Info("startup order action", "action", "skip_cancel_protected", "order_id", order.ID, "reason", reason)
		return false, nil
	}
	logger.Info("startup order action", "action", "cancel", "order_id", order.ID, "side", order.Side, "price", order.Price, "size", order.Size, "reason", reason)
	if cfg.DryRun {
		return true, nil
	}
	if err := client.CancelOrder(ctx, order.ID, reason); err != nil {
		return false, fmt.Errorf("cancel startup order %s: %w", order.ID, err)
	}
	return true, nil
}
