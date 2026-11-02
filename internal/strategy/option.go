package strategy

import (
	"math"
	"time"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/pricing"
	"github.com/numofx/market-maker/internal/state"
)

const (
	reasonOptionStateMissing = "option_state_unavailable"
	reasonOptionTerms        = "option_terms_missing"
	reasonVolUnavailable     = "vol_surface_unavailable"
	reasonVolDegraded        = "vol_surface_degraded"
	reasonOptionCutoff       = "option_quoting_cutoff"
	reasonOptionNoPremium    = "option_bid_below_tick"
	reasonOptionMaxPosition  = "option_max_position"
	reasonOptionCapacity     = "option_margin_capacity"
	reasonOperatorDisabled   = "operator_disabled"

	// minQuoteVol floors the bid's vol: a bid priced at zero or negative vol is not a price.
	minQuoteVol = 0.01
	// fullConfidence is the vol feed's confidence when the publisher is not degraded (1e18 on chain).
	fullConfidence = 1 - 1e-9
)

// buildOptionQuotes prices one strike the way MarginEngine v2 marks it -- Black-76 on the engine's forward and the
// strike's surface vol, at the block's time -- and quotes each side HalfSpreadVol off that vol (never closer than
// MinHalfSpread of premium), priced at a forward skewed against the process's net delta. Sizes are capped by the
// position limit, the account's IM surplus (a written side) and the premium it can pay (a bought side).
//
// It pulls the strike when the chain state is missing, the vol feed has no live surface, the series is inside its
// quoting cutoff, or the surface is degraded and DegradedSpreadMultiplier is 0; otherwise a degraded surface widens
// both sides by that multiplier.
func buildOptionQuotes(cfg config.Config, spec exchange.MarketSpec, snapshot state.Snapshot, ov Overrides, now time.Time) Result {
	oc := cfg.Option
	var result Result
	off := func(reason string) Result {
		result.Bid, result.Ask, result.Bids, result.Asks = nil, nil, nil, nil
		result.BidSuppression = &Suppression{Side: exchange.SideBuy, Reason: reason, Market: spec.Symbol, SubaccountID: cfg.SubaccountID}
		result.AskSuppression = &Suppression{Side: exchange.SideSell, Reason: reason, Market: spec.Symbol, SubaccountID: cfg.SubaccountID}
		return result
	}
	o, terms := snapshot.Option, spec.Option
	switch {
	case terms == nil || !(terms.UnitBase > 0):
		return off(reasonOptionTerms)
	case o == nil:
		return off(reasonOptionStateMissing)
	case o.VolUnavailable || !(o.Vol > 0) || !(o.Forward > 0):
		return off(reasonVolUnavailable)
	}
	cutoff := terms.LastTradeTime - oc.CutoffSeconds
	if o.BlockTime >= cutoff || now.Unix() >= cutoff {
		return off(reasonOptionCutoff)
	}
	degraded := o.VolConfidence < fullConfidence
	widen := 1.0
	if degraded {
		if oc.DegradedSpreadMultiplier == 0 {
			return off(reasonVolDegraded)
		}
		widen = oc.DegradedSpreadMultiplier
	}

	seconds := float64(terms.Expiry - o.BlockTime)
	fair := pricing.Option{Forward: o.Forward, Strike: terms.Strike, Vol: o.Vol, Seconds: seconds, Call: terms.Call}
	result.ReferencePrice = fair.Price()
	result.ReferenceSource = "black76"
	if !(result.ReferencePrice > 0) {
		return off(reasonVolUnavailable)
	}

	// Long delta prices at a lower forward: calls cheaper, puts dearer, so the fills that come reduce it.
	skew := oc.DeltaSkewBPS / 10_000 * math.Max(-1, math.Min(1, o.PortfolioDelta/oc.DeltaFullAt))
	result.SkewBPS = skew * 10_000
	quoted := fair
	quoted.Forward = o.Forward * (1 - skew)
	mid := quoted.Price()
	half := oc.HalfSpreadVol * widen
	minHalf := oc.MinHalfSpread * widen
	bidPrice := roundDown(math.Min(quoted.AtVol(math.Max(o.Vol-half, minQuoteVol)).Price(), mid-minHalf), spec.TickSize)
	askPrice := roundUp(math.Max(quoted.AtVol(o.Vol+half).Price(), mid+minHalf), spec.TickSize)

	inventory := snapshot.Inventory(spec.BaseAsset)
	size := oc.OrderSize * ov.SizeMult
	// A bought side pays premium out of the IM surplus; a written side needs WriteIMPerBase a cNGN of it. Closing an
	// existing position needs neither.
	surplus := math.Max(0, o.IMSurplus) * oc.IMUsage
	bidCap, askCap := math.Max(0, -inventory), math.Max(0, inventory)
	if bidPrice > 0 {
		bidCap += surplus / bidPrice
	}
	if o.WriteIMPerBase > 0 {
		askCap += surplus / o.WriteIMPerBase
	}
	bidRoom, askRoom := oc.MaxPosition-inventory, oc.MaxPosition+inventory

	side := func(sideName exchange.Side, price, room, capacity float64, disabled bool) (*Quote, *Suppression) {
		suppress := func(reason string) (*Quote, *Suppression) {
			return nil, &Suppression{Side: sideName, Reason: reason, Market: spec.Symbol, SubaccountID: cfg.SubaccountID,
				ReferencePrice: result.ReferencePrice, ReferenceSource: result.ReferenceSource, ConfiguredOrderSize: oc.OrderSize,
				CandidateSize: math.Min(size, math.Min(room, capacity)), AvailableCapacity: capacity, Inventory: inventory,
				MaxInventory: oc.MaxPosition, MinOrderSize: terms.UnitBase, SizeStep: terms.UnitBase, BaseAsset: spec.BaseAsset, QuoteAsset: spec.QuoteAsset}
		}
		switch {
		case disabled:
			return suppress(reasonOperatorDisabled)
		case !(price > 0):
			return suppress(reasonOptionNoPremium)
		case room < terms.UnitBase:
			return suppress(reasonOptionMaxPosition)
		}
		q := roundDown(math.Min(size, math.Min(room, capacity)), terms.UnitBase)
		if q < terms.UnitBase {
			return suppress(reasonOptionCapacity)
		}
		return &Quote{Price: price, Size: q}, nil
	}
	result.Bid, result.BidSuppression = side(exchange.SideBuy, bidPrice, bidRoom, bidCap, ov.BidDisabled)
	result.Ask, result.AskSuppression = side(exchange.SideSell, askPrice, askRoom, askCap, ov.AskDisabled)
	if result.Bid != nil {
		result.Bids = []Quote{*result.Bid}
	}
	if result.Ask != nil {
		result.Asks = []Quote{*result.Ask}
	}
	return result
}
