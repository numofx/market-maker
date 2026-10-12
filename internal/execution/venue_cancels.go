package execution

import (
	"context"

	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/state"
)

// cancelReasonReader is the client's read of why an order left the book (exchange.HTTPClient.
// CancelReasons). A client without it (a test double, an older build) leaves fill accounting as it was.
type cancelReasonReader interface {
	CancelReasons(ctx context.Context, orderIDs []string) (map[string]string, error)
}

// noteVenueCancels finds the orders that left the book since the last observation without this bot
// cancelling them, and asks the venue why. Every one the venue cancelled, for any reason, is returned
// with the bot's own cancels, so it is not counted as a fill.
//
// An insufficient_margin cancel on the MarginEngine future needs nothing more here: the future is sized
// by the account's IM headroom every cycle (strategy.marginEngineFutureCapacity), so the side is not
// requoted while the account cannot carry an order, and is requoted at what it can carry once margin
// returns.
func (b *Bot) noteVenueCancels(ctx context.Context, previous, current state.Snapshot, ours map[string]struct{}) map[string]struct{} {
	reader, ok := b.client.(cancelReasonReader)
	if !ok {
		return ours
	}
	still := make(map[string]struct{}, len(current.OpenOrders))
	for _, order := range current.OpenOrders {
		still[order.ID] = struct{}{}
	}
	var gone []exchange.Order
	for _, order := range previous.OpenOrders {
		if _, ok := still[order.ID]; ok {
			continue
		}
		if _, ok := ours[order.ID]; ok {
			continue
		}
		gone = append(gone, order)
	}
	if len(gone) == 0 {
		return ours
	}
	ids := make([]string, len(gone))
	for i, order := range gone {
		ids[i] = order.ID
	}
	reasons, err := reader.CancelReasons(ctx, ids)
	if err != nil {
		b.logger.Warn("venue cancel reasons unreadable", "orders", len(ids), "error", err)
		return ours
	}
	excluded := make(map[string]struct{}, len(ours)+len(gone))
	for id := range ours {
		excluded[id] = struct{}{}
	}
	for _, order := range gone {
		if reason := reasons[order.ID]; reason != "" {
			excluded[order.ID] = struct{}{}
			b.logger.Warn("order cancelled by the venue", "order_id", order.ID, "side", order.Side, "reason", reason)
		}
	}
	return excluded
}
