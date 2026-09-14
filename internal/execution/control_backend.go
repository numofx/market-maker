package execution

import (
	"context"
	"time"

	"github.com/numofx/market-maker/internal/control"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/strategy"
)

// AttachController wires the operator controller into the bot and its syncer. Call it before the
// control API starts and before the quoting loop runs; it is not synchronized with either.
func (b *Bot) AttachController(c *control.Controller) {
	b.control = c
	b.syncer.control = c
	b.publishView()
}

// View implements control.Backend. It is called from HTTP goroutines, so it reads only the copy the
// loop published under viewMu -- never the loop's own fields.
func (b *Bot) View() control.BotView {
	b.viewMu.Lock()
	view := b.view
	b.viewMu.Unlock()
	// The kill-switch file is read live rather than from the last cycle: a file dropped a second ago
	// must read KILLED now, and a resume must be refused against the file as it is, not as it was.
	// Safe off the loop: it reads only cfg, which never changes.
	if active, err := b.killSwitchActive(); err == nil {
		view.KillSwitchFileActive = active
		switch {
		case !active:
			view.KillSwitchFileSince = time.Time{}
		case view.KillSwitchFileSince.IsZero():
			view.KillSwitchFileSince = time.Now().UTC()
		}
	}
	return view
}

// CancelManaged implements control.Backend: the same cancel-everything a halt uses, run now from the
// handler rather than on the next cycle, with per-order results.
//
// It runs concurrently with the quoting loop. That is safe for the syncer (its mutable state is
// locked) and for the HTTP client; what it cannot do alone is stop a cycle that is already past its
// kill check from placing behind it. The controller flag, set before this is called, does that at
// place time -- see Controller.AllowPlace.
func (b *Bot) CancelManaged(ctx context.Context, scope control.CancelScope, side exchange.Side) ([]control.CancelResult, error) {
	category := ""
	switch scope {
	case control.CancelScopeKill:
		category = cancelCategoryKillSwitch
	case control.CancelScopePause:
		// The operator-pause path has always cancelled uncategorized; a control pause is the same act.
		category = ""
	case control.CancelScopeSide:
		category = cancelCategoryControlSide
	}
	return b.syncer.CancelAllWithResults(ctx, b.marketSymbol, category, side)
}

func (b *Bot) strategyOverrides() strategy.Overrides {
	st := b.control.Status()
	return strategy.Overrides{
		BidDisabled:  !st.BidEnabled,
		AskDisabled:  !st.AskEnabled,
		MidShiftBPS:  st.Adjust.MidShiftBPS,
		SpreadAddBPS: st.Adjust.SpreadAddBPS,
		SizeMult:     st.Adjust.SizeMult,
	}
}

func (b *Bot) finishCycle(err error) {
	b.lastCycleAt = time.Now().UTC()
	b.lastCycleErr = ""
	if err != nil {
		b.lastCycleErr = err.Error()
	}
	b.publishView()
}

// markRunning records the transition out of a halt, so /control/state can say since when the bot
// has been quoting.
func (b *Bot) markRunning() {
	if b.currentHalted || b.runningSince.IsZero() {
		b.runningSince = time.Now().UTC()
	}
	b.currentHalted = false
}

// publishView copies what /control/state shows into the view the control API reads. Called by the
// loop at the end of every cycle and of Initialize. Every slice and map is freshly built, so the
// published copy shares nothing the loop will later mutate.
func (b *Bot) publishView() {
	view := control.BotView{
		Market:              b.marketSymbol,
		OperatorMode:        string(b.cfg.OperatorMode),
		DryRun:              b.cfg.DryRun,
		Initialized:         b.initialized,
		KillSwitchFileSince: b.killFileSince,
		Halted:              b.currentHalted,
		HaltSince:           b.haltSince,
		RunningSince:        b.runningSince,
		LastCycleAt:         b.lastCycleAt,
		LastCycleError:      b.lastCycleErr,
		ReferencePrice:      b.snapshot.ReferencePrice,
		ReferenceSource:     b.snapshot.ReferenceSource,
		BestBid:             b.snapshot.BestBid,
		BestAsk:             b.snapshot.BestAsk,
		TargetBids:          levelsView(b.lastQuotes.Bids, b.lastQuotes.Bid),
		TargetAsks:          levelsView(b.lastQuotes.Asks, b.lastQuotes.Ask),
		OpenOrders:          make([]control.OpenOrder, 0, len(b.snapshot.OpenOrders)),
		Positions:           make(map[string]control.Position, len(b.snapshot.Positions)),
		Config: control.ConfigView{
			HalfSpreadBPS:              b.cfg.HalfSpreadBPS,
			QuoteLevels:                b.cfg.QuoteLevels,
			OrderSize:                  b.cfg.OrderSize,
			OrderExpirySeconds:         b.cfg.OrderExpirySeconds,
			ExpiryReplaceMarginSeconds: b.cfg.ExpiryReplaceMarginSeconds,
			PostOnly:                   b.cfg.PostOnlyQuotes,
			MaxNetInventory:            b.cfg.MaxNetInventory,
			MaxNotionalPerSide:         b.cfg.MaxNotionalPerSide,
		},
		HaltCount:      b.haltCount,
		LastHaltReason: b.persisted.LastHaltReason,
	}
	if b.currentHalted {
		view.HaltReason = b.persisted.LastHaltReason
	}
	for _, order := range b.snapshot.OpenOrders {
		view.OpenOrders = append(view.OpenOrders, control.OpenOrder{
			OrderID:   order.ID,
			Side:      string(order.Side),
			Price:     order.Price,
			Size:      order.Size,
			Nonce:     order.Nonce,
			CreatedAt: order.CreatedAt,
			Expiry:    order.Expiry,
			PostOnly:  order.PostOnly,
		})
	}
	for asset, position := range b.snapshot.Positions {
		view.Positions[asset] = control.Position{Total: position.Total, Reserved: position.Reserved, Available: position.Available}
	}
	b.viewMu.Lock()
	b.view = view
	b.viewMu.Unlock()
}

func levelsView(ladder []strategy.Quote, best *strategy.Quote) []control.Level {
	if len(ladder) == 0 && best != nil {
		ladder = []strategy.Quote{*best}
	}
	out := make([]control.Level, 0, len(ladder))
	for _, quote := range ladder {
		out = append(out, control.Level{Price: quote.Price, Size: quote.Size})
	}
	return out
}

func anyExpiring(orders []exchange.Order, marginSeconds int64, now time.Time) bool {
	for i := range orders {
		if expiringWithin(&orders[i], marginSeconds, now) {
			return true
		}
	}
	return false
}
