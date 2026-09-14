package execution

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"sort"
	"strings"
	"time"

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
		reason := startupRejectReason(cfg, spec, order, client)
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

func startupRejectReason(cfg config.Config, spec exchange.MarketSpec, order exchange.Order, client exchange.Client) string {
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
	required, err := client.RequiredWorstFee(spec, order.Price)
	if err != nil {
		required = ""
	}
	return staleTermsReason(cfg, &order, required)
}

// staleTermsReason rejects an order that is resting under terms this process would not sign now.
//
// An order carries the guarantees of the image that placed it, and nothing else notices. A stable
// ladder is never replaced -- that is what the quantum fix achieved -- so a quote placed by a
// previous image can rest indefinitely under superseded terms while the operator believes the new
// ones are in force.
//
// That is not hypothetical. Deploying post-only left six live quotes without it for twenty
// minutes: the bot had the flag, the ladder never churned, and only reading post_only back off
// the venue showed it. A restart fixed it by accident. This makes the restart do it on purpose.
//
// Two terms matter, because both are promises made at signing time and neither can be amended
// afterwards:
//
//   - post_only: an order without it can take, whatever the config says.
//   - worstFee: the signed per-unit bound. One signed under an older, lower fee schedule reverts
//     TM_FeeTooHigh on every fill it takes, silently, until it expires.
//
// staleTermsReason reports why an order is resting under terms this process would not sign now.
//
// Takes the required bound rather than a client, so the same rule serves both startup
// reconciliation and the steady-state cycle. Empty requiredWorstFee means "cannot say", which
// leaves the order alone -- the schedule refresh forces a requote if the fee actually moved, and
// churning the whole book on a failed lookup would be worse than a stale bound.
//
// Three terms matter, because all are promises made at signing time that cannot be amended:
//
//   - post_only: an order without it can take, whatever the config says.
//   - worstFee: a bound signed under an older, lower schedule reverts TM_FeeTooHigh on every fill
//     it takes, silently, until it expires.
//   - expiry: see expiryBeyondConfig.
func staleTermsReason(cfg config.Config, order *exchange.Order, requiredWorstFee string) string {
	if order == nil {
		return ""
	}
	if order.PostOnly != cfg.PostOnlyQuotes {
		return "post_only_mismatch"
	}
	if expiryBeyondConfig(cfg, order, time.Now()) {
		return "expiry_beyond_config"
	}
	if strings.TrimSpace(requiredWorstFee) == "" {
		return ""
	}
	have, ok := new(big.Int).SetString(strings.TrimSpace(order.WorstFee), 10)
	if !ok {
		return "unreadable_worst_fee"
	}
	want, ok := new(big.Int).SetString(strings.TrimSpace(requiredWorstFee), 10)
	if !ok {
		return ""
	}
	if have.Cmp(want) < 0 {
		return "worst_fee_below_current_schedule"
	}
	return ""
}

// signedExpirySlackSeconds absorbs clock skew between the process that signed an order and this one.
const signedExpirySlackSeconds = 10

// expiryBeyondConfig reports an order signed to live longer than this process would sign now.
//
// A cancel is off-chain only: TradeModule has no nonce invalidation, so the signed expiry is the
// only bound on how long a pulled quote can still settle. The image before 60s expiry signed 3600s,
// and its ladder survives the deploy. Without this, startup adopts those quotes (price, size and
// the other terms all match) and the expiry margin leaves them until they are nearly an hour old --
// an hour of exposure while the config says sixty seconds. Lowering MM_ORDER_EXPIRY_SECONDS later
// has the same shape.
//
// Unknown expiry (0) is left alone, as in expiringWithin. So are protected orders (validation:,
// test:, ...): the bot never cancels them, and cmd/acceptance-cross still signs validation: orders
// for 3600s. Flagging one would only free its slot and place a duplicate beside it on every poll.
func expiryBeyondConfig(cfg config.Config, order *exchange.Order, now time.Time) bool {
	if order.Expiry <= 0 || cfg.OrderExpirySeconds <= 0 || isProtectedOrderID(cfg, order.ID) {
		return false
	}
	return order.Expiry > now.Unix()+cfg.OrderExpirySeconds+signedExpirySlackSeconds
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
