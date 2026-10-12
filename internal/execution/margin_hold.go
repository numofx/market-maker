package execution

import (
	"context"
	"time"

	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/state"
	"github.com/numofx/market-maker/internal/strategy"
)

// futureMarginReader is what the margin hold needs from the client: why an order left the book, and
// the account's IM headroom on the market's manager. The HTTP client implements it; a client that
// does not (a test double, an older build) leaves the bot as it was: every venue cancel is requoted at
// once.
type futureMarginReader interface {
	CancelReasons(ctx context.Context, orderIDs []string) (map[string]string, error)
	FutureIMHeadroom(ctx context.Context, spec exchange.MarketSpec) (surplus float64, perContract float64, err error)
}

// marginHoldMax bounds a hold whose headroom cannot be read: the side is requoted after it and the
// venue's own check decides, so an unreadable chain cannot keep a side off the book indefinitely.
const marginHoldMax = 15 * time.Minute

// noteVenueCancels finds the orders that left the book since the last snapshot without this bot
// cancelling them, and asks the venue why. One the venue cancelled for insufficient margin puts its
// side on hold: requoting it at once would only rest an order the venue refuses again and cancels a
// minute later, round and round until the account is topped up. Every order the venue cancelled, for
// any reason, is returned so it is not counted as a fill.
func (b *Bot) noteVenueCancels(ctx context.Context, previous, current state.Snapshot, ours map[string]struct{}) map[string]struct{} {
	reader, ok := b.client.(futureMarginReader)
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
		reason := reasons[order.ID]
		if reason == "" {
			continue
		}
		excluded[order.ID] = struct{}{}
		b.logger.Warn("order cancelled by the venue", "order_id", order.ID, "side", order.Side, "reason", reason)
		if reason != exchange.CancelReasonInsufficientMargin {
			continue
		}
		if b.marginHolds == nil {
			b.marginHolds = map[exchange.Side]time.Time{}
		}
		if _, held := b.marginHolds[order.Side]; !held {
			b.marginHolds[order.Side] = time.Now().UTC()
			b.logger.Warn("side held for margin", "side", order.Side, "until", "the account's IM headroom covers one order")
		}
	}
	return excluded
}

// applyMarginHolds takes a held side off the quotes until the account can carry one of its orders:
// an IM surplus of at least the best level's size x the IM a contract adds. Released, the side is
// quoted this cycle.
func (b *Bot) applyMarginHolds(ctx context.Context, quotes *strategy.Result) {
	if len(b.marginHolds) == 0 {
		return
	}
	reader, ok := b.client.(futureMarginReader)
	if !ok {
		return
	}
	surplus, perContract, err := reader.FutureIMHeadroom(ctx, b.spec)
	for side, since := range b.marginHolds {
		best := quotes.Bid
		if side == exchange.SideSell {
			best = quotes.Ask
		}
		switch {
		case err != nil && time.Since(since) >= marginHoldMax:
			b.logger.Warn("margin hold released unverified", "side", side, "held_for", time.Since(since).Round(time.Second), "error", err)
			delete(b.marginHolds, side)
			continue
		case err != nil:
			b.logger.Warn("margin hold headroom unreadable", "side", side, "error", err)
		case best == nil || surplus >= best.Size*perContract:
			b.logger.Info("margin hold released", "side", side, "im_surplus", surplus, "held_for", time.Since(since).Round(time.Second))
			delete(b.marginHolds, side)
			continue
		}
		suppression := &strategy.Suppression{Side: side, Reason: exchange.CancelReasonInsufficientMargin, Market: b.spec.Symbol,
			AvailableCapacity: surplus, RequiredCapacity: perContract}
		if best != nil {
			suppression.EffectiveOrderSize = best.Size
			suppression.RequiredCapacity = best.Size * perContract
		}
		if side == exchange.SideSell {
			quotes.Ask, quotes.Asks, quotes.AskSuppression = nil, nil, suppression
		} else {
			quotes.Bid, quotes.Bids, quotes.BidSuppression = nil, nil, suppression
		}
	}
}
