package strategy

import (
	"fmt"
	"math"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/state"
)

type Quote struct {
	Side  exchange.Side
	Price float64
	Size  float64
}

type Suppression struct {
	Side                 exchange.Side
	Reason               string
	Market               string
	SubaccountID         string
	AnchorPrice          float64
	ReferencePrice       float64
	ReferenceSource      string
	RequiredCapacity     float64
	TotalCapacity        float64
	ReservedCapacity     float64
	AvailableCapacity    float64
	ConfiguredOrderSize  float64
	EffectiveOrderSize   float64
	MinOrderSize         float64
	CandidateSize        float64
	SizeStep             float64
	Inventory            float64
	MaxInventory         float64
	SpotAssetAddress     string
	QuoteAssetAddress    string
	BaseAsset            string
	QuoteAsset           string
	DependencyStale      bool
	ExternalAnchorFailed bool
	DryRun               bool
	OperatorMode         string
}

type Result struct {
	ReferencePrice       float64
	ReferenceSource      string
	LocalReferencePrice  float64
	LocalReferenceSource string
	AnchorPrice          float64
	Bid                  *Quote
	Ask                  *Quote
	// Bids/Asks are the full ladder, best (innermost) first. With MM_QUOTE_LEVELS=1
	// each holds exactly the single Bid/Ask, so the syncer's per-level loop degrades
	// to the original one-level behavior. Bid/Ask alias the best level for callers
	// (metrics, logs, cross-quote checks) that only care about the top of book.
	Bids           []Quote
	Asks           []Quote
	BidSuppression *Suppression
	AskSuppression *Suppression
	SkewBPS        float64
}

func ComputeReferencePrice(snapshot state.Snapshot) (float64, string) {
	if snapshot.Perp != nil {
		// The loader already chose: other traders' mid inside the basis band, else the index.
		return snapshot.Perp.Reference, snapshot.Perp.ReferenceSource
	}
	if snapshot.Market == "USDCcNGN-SPOT" {
		// Other participants' two-sided book first; then the external fallback price; the venue's own last
		// trade only when neither exists. A thin venue's last print can be hours old and far from the market
		// (1327 against ~1371 on 2026-09-14), while the fallback is a live cross-venue rate.
		localRef, localSource := ComputeLocalReference(snapshot)
		if localRef > 0 && localSource == "book" {
			return localRef, localSource
		}
		if snapshot.ExternalAnchorPrice > 0 {
			return snapshot.ExternalAnchorPrice, "external"
		}
		if localRef > 0 {
			return localRef, localSource
		}
		return 0, "none"
	}
	if snapshot.AnchorPrice > 0 {
		return snapshot.AnchorPrice, "none"
	}
	localRef := ComputeLocalReferencePrice(snapshot)
	if localRef > 0 {
		return localRef, snapshot.LocalReferenceSource
	}
	return 0, "none"
}

func ComputeLocalReferencePrice(snapshot state.Snapshot) float64 {
	price, _ := ComputeLocalReference(snapshot)
	return price
}

func ComputeLocalReference(snapshot state.Snapshot) (float64, string) {
	if snapshot.LocalReferencePrice > 0 {
		if snapshot.LocalReferenceSource == "" {
			return snapshot.LocalReferencePrice, "book"
		}
		return snapshot.LocalReferencePrice, snapshot.LocalReferenceSource
	}
	if snapshot.BestBid > 0 && snapshot.BestAsk > 0 {
		return (snapshot.BestBid + snapshot.BestAsk) / 2, "book"
	}
	if price, ok := state.ReferenceTradePrice(snapshot); ok {
		return price, "trade"
	}
	return 0, "none"
}

// Overrides are the operator's runtime adjustments from the control API, applied on top of config.
//
// The zero value is NOT neutral -- SizeMult 0 means "quote zero size", which is a legitimate
// adjustment -- so callers with nothing to override use NoOverrides.
type Overrides struct {
	// BidDisabled/AskDisabled pull one side, through the same suppression path as bid-only and
	// ask-only.
	BidDisabled bool
	AskDisabled bool
	// MidShiftBPS moves the pricing mid by this many bps OF the mid; positive raises both bid and ask.
	MidShiftBPS float64
	// SpreadAddBPS is added to each side's half spread, after the external-anchor multiplier, so the
	// number the operator typed is the number of bps each quote moves.
	SpreadAddBPS float64
	// SizeMult scales the configured order size before capacity and inventory caps, which still bind.
	SizeMult float64
}

// NoOverrides is the neutral set: both sides on, no shift, no added spread, size unchanged.
func NoOverrides() Overrides {
	return Overrides{SizeMult: 1}
}

func BuildQuotes(cfg config.Config, spec exchange.MarketSpec, snapshot state.Snapshot) (Result, error) {
	return BuildQuotesWithOverrides(cfg, spec, snapshot, NoOverrides())
}

// BuildQuotesWithOverrides is BuildQuotes with the operator's runtime adjustments applied where the
// strategy prices: the mid shift to the reference the ladder is built around, the spread add to the
// half spread, and the size multiplier to the order size.
//
// Result.ReferencePrice stays the UNSHIFTED market reference. It feeds risk (reference available?),
// the quoted-spread metric, and /control/state, all of which are about the market, not about where
// the operator chose to lean.
func BuildQuotesWithOverrides(cfg config.Config, spec exchange.MarketSpec, snapshot state.Snapshot, ov Overrides) (Result, error) {
	ref, refSource := ComputeReferencePrice(snapshot)
	localRef, localSource := ComputeLocalReference(snapshot)
	result := Result{
		ReferencePrice:       ref,
		ReferenceSource:      refSource,
		LocalReferencePrice:  localRef,
		LocalReferenceSource: localSource,
		AnchorPrice:          snapshot.AnchorPrice,
	}
	if ref <= 0 {
		result.BidSuppression = baseSuppression(cfg, spec, snapshot, exchange.SideBuy, "no_anchor", 0, 0, 0, 0, 0, 0, 0, 0, false)
		result.AskSuppression = baseSuppression(cfg, spec, snapshot, exchange.SideSell, "no_anchor", 0, 0, 0, 0, 0, 0, 0, 0, false)
		return result, nil
	}

	// Inventory is the base asset in the engine's units: cNGN held on spot, the signed cNGN
	// position on the perp (long cNGN positive), contracts on a future. The operator's limits are
	// USDC on the cNGN markets, so they are converted to cNGN at the market reference here, once.
	inventory := snapshot.Inventory(spec.BaseAsset)
	maxLong, maxShort := inventoryLimits(cfg, spec, ref)
	skewBPS := inventorySkew(inventory, maxLong, maxShort, cfg.InventorySkewBPS)
	halfSpreadBPS := cfg.HalfSpreadBPS
	orderSize := cfg.OrderSize
	if snapshot.Market == "USDCcNGN-SPOT" && refSource == "external" {
		halfSpreadBPS *= cfg.USDCCNGNSpotExternalAnchor.SpreadMultiplier
		orderSize *= cfg.USDCCNGNSpotExternalAnchor.SizeMultiplier
	}
	halfSpreadBPS += ov.SpreadAddBPS
	orderSize *= ov.SizeMult
	// MM_ORDER_SIZE is USDC per rung on the cNGN markets; a rung is placed as whole cNGN.
	orderSize = baseSize(spec, orderSize, ref)
	halfSpread := halfSpreadBPS / 10000.0
	skew := skewBPS / 10000.0
	pricingRef := ref * (1 + ov.MidShiftBPS/10000.0)

	bidPrice := roundDown(pricingRef*(1-halfSpread-skew), spec.TickSize)
	askPrice := roundUp(pricingRef*(1+halfSpread-skew), spec.TickSize)
	if bidPrice <= 0 || askPrice <= 0 || bidPrice >= askPrice || !isFinite(bidPrice) || !isFinite(askPrice) {
		return result, fmt.Errorf("calculated invalid quote prices")
	}

	basePosition := snapshot.Position(spec.BaseAsset)
	quotePosition := snapshot.Position(spec.QuoteAsset)

	// A cash-margined future is settled/collateralized in the quote (cash) asset: SELLING opens a
	// SHORT backed by cash margin rather than delivering held base-asset inventory. Its ask is gated
	// by cash/quote capacity and the short inventory limit, mirroring the bid, NOT by base-asset
	// availability. Spot still sells held base inventory (baseAvailable).
	cashMarginedFuture := isCashMarginedFuture(spec)

	var baseAvailable, quoteAvailable float64
	if cashMarginedFuture {
		// The cash asset is the SHARED margin collateral for both sides, and the MM cancel-replaces
		// its own resting orders every cycle, so the budget is the full cash Total (= Available + the
		// margin its own resting orders reserve). Do NOT add reusableCapacity: it re-derives capacity
		// from order NOTIONAL (size*price), but a future order reserves only MARGIN (a fraction of
		// notional), so adding notional inflates quoteAvailable every cycle and the quote size runs
		// away without bound. Total is stable; MaxNotionalPerSide/inventory caps bound the per-side size.
		quoteAvailable = quotePosition.Total
		baseAvailable = 0
	} else {
		// Budget = Available + Reusable, both reported by the client.
		//
		// Reusable is the part of Reserved this bot frees again this cycle: its own replaceable
		// orders on THIS market. The client computes it with the same arithmetic that produced
		// Reserved, which is the point -- the previous version recomputed the reservation here,
		// from UI values (USDC size x cNGN-per-USDC) while the client used engine values (cNGN
		// amount x USDC-per-cNGN). Those are not the same quantity, so the add-back never
		// cancelled the subtraction, and the residual fed the next cycle's size -- which changed
		// the orders, which changed the reservation. The target walked while the price stood still.
		//
		// Measured before this change: 62 of 71 consecutive replacements had current_size equal to
		// the PREVIOUS cycle's target_size, drifting ~1.1% per cycle at a price identical to 15
		// digits, with diffs 18x to 1152x the size quantum. Not rounding, and not the market.
		//
		// Not Total, either: the exposure query filters by owner and subaccount but NOT by market,
		// so Reserved can include orders on another market that this ladder cannot free, and
		// protected orders are never cancelled by design. Reusable excludes both.
		baseAvailable = basePosition.Available + basePosition.Reusable
		quoteAvailable = quotePosition.Available + quotePosition.Reusable
	}

	// Capacity in base units: a bid can buy quoteAvailable / price of the base, an ask can sell
	// what is held. The same arithmetic in every orientation -- the quote is USDC and the price is
	// USDC per cNGN, so the quotient is cNGN.
	maxBidSize := quoteAvailable / bidPrice
	maxAskSize := baseAvailable
	if cashMarginedFuture {
		maxAskSize = quoteAvailable / askPrice
	}
	if spec.IsPerp() && snapshot.Perp != nil {
		// The perp's cash is USDC and the position is cNGN: capacity is a leverage bound in USDC,
		// converted to cNGN at the reference.
		maxBidSize, maxAskSize = perpCapacity(cfg, *snapshot.Perp, quotePosition.Total, inventory, ref)
	}
	if cfg.MaxNotionalPerSide > 0 {
		// On the cNGN markets MM_MAX_NOTIONAL_PER_SIDE is a cNGN amount, so it caps the base size
		// directly; on a future it is quote notional and divides by the price.
		if spec.CNGNDenominated() {
			maxBidSize = minFloat(maxBidSize, cfg.MaxNotionalPerSide)
			maxAskSize = minFloat(maxAskSize, cfg.MaxNotionalPerSide)
		} else {
			maxBidSize = minFloat(maxBidSize, cfg.MaxNotionalPerSide/bidPrice)
			maxAskSize = minFloat(maxAskSize, cfg.MaxNotionalPerSide/askPrice)
		}
	}
	bidSize := roundDown(minFloat(orderSize, maxBidSize), spec.SizeStep)
	askSize := roundDown(minFloat(orderSize, maxAskSize), spec.SizeStep)

	bidMinSize := minQuoteSize(spec)
	askMinSize := minQuoteSize(spec)
	if bidSize >= bidMinSize && inventory+bidSize <= maxLong {
		result.Bid = &Quote{Side: exchange.SideBuy, Price: bidPrice, Size: bidSize}
		result.Bids = buildLevels(cfg, spec, exchange.SideBuy, pricingRef, halfSpread, skew, orderSize, maxBidSize, inventory, maxLong, askPrice)
	} else {
		result.BidSuppression = baseSuppression(cfg, spec, snapshot, exchange.SideBuy, bidSuppressionReason(orderSize, bidSize, bidMinSize, maxBidSize, quoteAvailable, inventory, maxLong), bidMinSize*bidPrice, quotePosition.Total, quotePosition.Reserved, quoteAvailable, orderSize, bidSize, bidPrice, maxLong, false)
	}
	if askSize >= askMinSize && inventory-askSize >= maxShort {
		result.Ask = &Quote{Side: exchange.SideSell, Price: askPrice, Size: askSize}
		result.Asks = buildLevels(cfg, spec, exchange.SideSell, pricingRef, halfSpread, skew, orderSize, maxAskSize, inventory, maxShort, bidPrice)
	} else if cashMarginedFuture {
		// Short backed by cash: report cash/quote capacity, not base-asset inventory.
		result.AskSuppression = baseSuppression(cfg, spec, snapshot, exchange.SideSell, futureAskSuppressionReason(orderSize, askSize, askMinSize, maxAskSize, quoteAvailable, inventory, maxShort), askMinSize*askPrice, quotePosition.Total, quotePosition.Reserved, quoteAvailable, orderSize, askSize, askPrice, maxShort, false)
	} else {
		result.AskSuppression = baseSuppression(cfg, spec, snapshot, exchange.SideSell, askSuppressionReason(orderSize, askSize, askMinSize, maxAskSize, baseAvailable, basePosition.Total, inventory, maxShort), askMinSize, basePosition.Total, basePosition.Reserved, baseAvailable, orderSize, askSize, askPrice, maxShort, false)
	}

	if spec.IsPerp() && (snapshot.Perp == nil || (!snapshot.Perp.TradingEnabled && !cfg.PerpQuoteWhileClosed)) {
		// Closed until the enable vault action: the matcher skips the market, so quotes would only
		// rest. MM_PERP_QUOTE_WHILE_CLOSED rests them anyway, for the launch: the enable gate needs a
		// two-sided book before it will open the market.
		result.Bid, result.Ask, result.Bids, result.Asks = nil, nil, nil, nil
		result.BidSuppression = baseSuppression(cfg, spec, snapshot, exchange.SideBuy, "perp_trading_disabled", spec.MinSize*bidPrice, quotePosition.Total, quotePosition.Reserved, quoteAvailable, orderSize, bidSize, bidPrice, maxLong, false)
		result.AskSuppression = baseSuppression(cfg, spec, snapshot, exchange.SideSell, "perp_trading_disabled", spec.MinSize*askPrice, quotePosition.Total, quotePosition.Reserved, quoteAvailable, orderSize, askSize, askPrice, maxShort, false)
		result.SkewBPS = skewBPS
		return result, nil
	}

	switch cfg.OperatorMode {
	case config.ModePause, config.ModeDryRunHealth:
		result.Bid = nil
		result.Ask = nil
		result.Bids = nil
		result.Asks = nil
		result.BidSuppression = baseSuppression(cfg, spec, snapshot, exchange.SideBuy, "operator_halted", spec.MinSize*bidPrice, quotePosition.Total, quotePosition.Reserved, quoteAvailable, orderSize, bidSize, bidPrice, maxLong, false)
		result.AskSuppression = baseSuppression(cfg, spec, snapshot, exchange.SideSell, "operator_halted", spec.MinSize, basePosition.Total, basePosition.Reserved, baseAvailable, orderSize, askSize, askPrice, maxShort, false)
	default:
		// bid-only/ask-only and a side pulled through the control API are the same suppression; only
		// the reason differs, so a log line says which of the two turned the side off.
		if cfg.OperatorMode == config.ModeBidOnly || ov.AskDisabled {
			result.Ask = nil
			result.Asks = nil
			result.AskSuppression = baseSuppression(cfg, spec, snapshot, exchange.SideSell, sideOffReason(cfg.OperatorMode == config.ModeBidOnly), spec.MinSize, basePosition.Total, basePosition.Reserved, baseAvailable, orderSize, askSize, askPrice, maxShort, false)
		}
		if cfg.OperatorMode == config.ModeAskOnly || ov.BidDisabled {
			result.Bid = nil
			result.Bids = nil
			result.BidSuppression = baseSuppression(cfg, spec, snapshot, exchange.SideBuy, sideOffReason(cfg.OperatorMode == config.ModeAskOnly), spec.MinSize*bidPrice, quotePosition.Total, quotePosition.Reserved, quoteAvailable, orderSize, bidSize, bidPrice, maxLong, false)
		}
	}
	result.SkewBPS = skewBPS
	return result, nil
}

func sideOffReason(byOperatorMode bool) string {
	if byOperatorMode {
		return "operator_halted"
	}
	return "control_side_disabled"
}

func bidSuppressionReason(orderSize, bidSize, minSize, maxBidSize, quoteAvailable, inventory, maxLong float64) string {
	if orderSize < minSize {
		return "min_order_size_not_met"
	}
	if maxBidSize < minSize || bidSize < minSize {
		return "insufficient_quote_capacity"
	}
	if inventory+bidSize > maxLong {
		return "max_long_inventory"
	}
	if quoteAvailable <= 0 {
		return "insufficient_quote_capacity"
	}
	return "bid_quote_suppressed"
}

// isCashMarginedFuture reports whether both sides of a quote are backed by cash margin rather than
// held inventory: every market but spot, i.e. the dated futures and the perp.
func isCashMarginedFuture(spec exchange.MarketSpec) bool {
	return spec.Symbol != "" && !spec.IsSpot()
}

// perpCapacity is the most each side may quote, in cNGN, on the perp:
//
//   - the bot's own leverage cap: its gross position after a fill stays within cash x
//     min(MM_PERP_MAX_LEVERAGE, the SRM's max leverage), a USDC bound converted to cNGN at the
//     reference price. A bid may first close a short and then open up to that bound long, and an
//     ask the reverse, so the limit is the bound minus the position on the side being added to;
//   - the OI cap: what opens a NEW position uses room under the cap, what closes one does not.
//     Not while the market is closed and MM_PERP_QUOTE_WHILE_CLOSED is set: a closed perp has a cap
//     of 0, so its room is 0 and the clamp would suppress exactly the quotes the flag exists to
//     rest (seen at launch, 2026-10-01). Those quotes cannot fill -- the matcher skips a closed
//     market -- and the clamp applies again on the first cycle after the cap opens.
//
// inventory is the signed engine position in cNGN (long cNGN positive), as PerpBalances reports it;
// cash is USDC; price is USDC per cNGN.
func perpCapacity(cfg config.Config, perp state.PerpSnapshot, cash, inventory, price float64) (maxBid, maxAsk float64) {
	if !(price > 0) {
		return 0, 0
	}
	leverage := cfg.PerpMaxLeverage
	if perp.MaxLeverage > 0 && perp.MaxLeverage < leverage {
		leverage = perp.MaxLeverage
	}
	bound := math.Max(0, cash) * leverage / price
	maxBid = math.Max(0, bound-inventory)
	maxAsk = math.Max(0, bound+inventory)
	if !perp.TradingEnabled && cfg.PerpQuoteWhileClosed {
		return maxBid, maxAsk
	}
	room := math.Max(0, perp.SideRoomNGN)
	maxBid = math.Min(maxBid, math.Max(0, -inventory)+room)
	maxAsk = math.Min(maxAsk, math.Max(0, inventory)+room)
	return maxBid, maxAsk
}

// baseSize converts an operator size to the market's base units: on the cNGN markets the operator
// configures USDC and the venue takes cNGN, so the size is divided by the reference price (USDC
// per cNGN); a future's size is already in contracts.
func baseSize(spec exchange.MarketSpec, usd, price float64) float64 {
	if !spec.CNGNDenominated() {
		return usd
	}
	if !(price > 0) {
		return 0
	}
	return usd / price
}

// inventoryLimits is the long and short inventory bound in base units. MM_MAX_LONG_INVENTORY,
// MM_MAX_SHORT_INVENTORY and MM_MAX_NET_INVENTORY are USDC on the cNGN markets (the value of the
// cNGN held, or of the perp position, at the reference), contracts on a future.
func inventoryLimits(cfg config.Config, spec exchange.MarketSpec, price float64) (maxLong, maxShort float64) {
	maxLong, maxShort = effectiveMaxLong(cfg), effectiveMaxShort(cfg)
	if spec.CNGNDenominated() {
		if !(price > 0) {
			return 0, 0
		}
		return maxLong / price, maxShort / price
	}
	return maxLong, maxShort
}

// futureAskSuppressionReason mirrors bidSuppressionReason for the short (sell) side of a
// cash-margined future: the ask is limited by cash/quote capacity and the short inventory
// limit, never by base-asset availability.
func futureAskSuppressionReason(orderSize, askSize, minSize, maxAskSize, quoteAvailable, inventory, maxShort float64) string {
	if orderSize < minSize {
		return "min_order_size_not_met"
	}
	if maxAskSize < minSize || askSize < minSize {
		return "insufficient_quote_capacity"
	}
	if inventory-askSize < maxShort {
		return "max_short_inventory"
	}
	if quoteAvailable <= 0 {
		return "insufficient_quote_capacity"
	}
	return "ask_quote_suppressed"
}

func askSuppressionReason(orderSize, askSize, minSize, maxAskSize, baseAvailable, baseTotal, inventory, maxShort float64) string {
	if orderSize < minSize {
		return "min_order_size_not_met"
	}
	if baseTotal <= 0 {
		return "missing_spot_asset_inventory"
	}
	if baseAvailable <= 0 {
		return "insufficient_base_capacity"
	}
	if maxAskSize < minSize || askSize < minSize {
		return "insufficient_base_capacity"
	}
	if inventory-askSize < maxShort {
		return "max_short_inventory"
	}
	return "ask_quote_suppressed"
}

func baseSuppression(cfg config.Config, spec exchange.MarketSpec, snapshot state.Snapshot, side exchange.Side, reason string, requiredCapacity, totalCapacity, reservedCapacity, availableCapacity, effectiveOrderSize, candidateSize, price, maxInventory float64, dependencyStale bool) *Suppression {
	return &Suppression{
		Side:                 side,
		Reason:               reason,
		Market:               spec.Symbol,
		SubaccountID:         cfg.SubaccountID,
		AnchorPrice:          snapshot.AnchorPrice,
		ReferencePrice:       ComputeReferencePriceValue(snapshot),
		ReferenceSource:      ComputeReferencePriceSource(snapshot),
		RequiredCapacity:     requiredCapacity,
		TotalCapacity:        totalCapacity,
		ReservedCapacity:     reservedCapacity,
		AvailableCapacity:    availableCapacity,
		ConfiguredOrderSize:  cfg.OrderSize,
		EffectiveOrderSize:   effectiveOrderSize,
		MinOrderSize:         spec.MinSize,
		CandidateSize:        candidateSize,
		SizeStep:             spec.SizeStep,
		Inventory:            snapshot.Inventory(spec.BaseAsset),
		MaxInventory:         maxInventory,
		SpotAssetAddress:     spec.AssetAddress,
		QuoteAssetAddress:    spec.QuoteAddress,
		BaseAsset:            spec.BaseAsset,
		QuoteAsset:           spec.QuoteAsset,
		DependencyStale:      dependencyStale,
		ExternalAnchorFailed: snapshot.ExternalAnchorRefreshFailed,
		DryRun:               cfg.DryRun,
		OperatorMode:         string(cfg.OperatorMode),
	}
}

func ComputeReferencePriceValue(snapshot state.Snapshot) float64 {
	price, _ := ComputeReferencePrice(snapshot)
	return price
}

func ComputeReferencePriceSource(snapshot state.Snapshot) string {
	_, source := ComputeReferencePrice(snapshot)
	return source
}

// buildLevels expands the single best-level quote into a ladder of up to
// cfg.QuoteLevels price points stepping outward from the reference by
// LevelSpreadStepBPS each, with per-level size scaled by LevelSizeMult^level.
// The cumulative size across all levels is bounded by the SAME budget and
// inventory limit the single-level path uses (totalBudget / invLimit), so
// depth grows without increasing net exposure. With QuoteLevels==1 the result
// is exactly [{best price, best size}] — identical to the pre-ladder behavior.
func buildLevels(cfg config.Config, spec exchange.MarketSpec, side exchange.Side, ref, halfSpread, skew, orderSize, totalBudget, inventory, invLimit, oppositePrice float64) []Quote {
	levels := cfg.QuoteLevels
	if levels < 1 {
		levels = 1
	}
	stepFrac := cfg.LevelSpreadStepBPS / 10000.0
	sizeMult := cfg.LevelSizeMult
	if sizeMult <= 0 {
		sizeMult = 1
	}

	// Remaining size budget (capacity/notional cap) and inventory headroom, both
	// shared across the whole side's ladder. Everything here is in base units (cNGN on the cNGN
	// markets): orderSize and invLimit were converted before the call.
	remainingBudget := totalBudget
	var remainingInv float64
	if side == exchange.SideBuy {
		remainingInv = invLimit - inventory // max long headroom
	} else {
		remainingInv = inventory - invLimit // max short headroom (invLimit is <= 0)
	}

	quotes := make([]Quote, 0, levels)
	levelSize := orderSize
	for k := 0; k < levels; k++ {
		if remainingBudget < spec.MinSize || remainingInv < spec.MinSize {
			break
		}
		var price float64
		if side == exchange.SideBuy {
			price = roundDown(ref*(1-halfSpread-skew-float64(k)*stepFrac), spec.TickSize)
			if price <= 0 || (oppositePrice > 0 && price >= oppositePrice) {
				break
			}
		} else {
			price = roundUp(ref*(1+halfSpread-skew+float64(k)*stepFrac), spec.TickSize)
			if price <= 0 || (oppositePrice > 0 && price <= oppositePrice) {
				break
			}
		}
		size := roundDown(minFloat(levelSize, minFloat(remainingBudget, remainingInv)), spec.SizeStep)
		if size < minQuoteSize(spec) {
			break
		}
		quotes = append(quotes, Quote{Side: side, Price: price, Size: size})
		remainingBudget -= size
		remainingInv -= size
		levelSize *= sizeMult
	}
	return quotes
}

// minQuoteSize is the smallest size the venue accepts. A cNGN-market order is a whole number of
// cNGN, so a side whose budget is under 1 cNGN cannot be quoted at all. Quoting one anyway failed
// the whole cycle -- both sides -- when an ask was left with 0.000682 USDC (under 1 cNGN at the
// time); below this size a side is suppressed instead, and the other side keeps quoting.
func minQuoteSize(spec exchange.MarketSpec) float64 {
	if spec.CNGNDenominated() {
		return math.Max(spec.MinSize, 1)
	}
	return spec.MinSize
}

func effectiveMaxLong(cfg config.Config) float64 {
	if cfg.MaxNetInventory > 0 && (cfg.MaxLongInventory == 0 || cfg.MaxNetInventory < cfg.MaxLongInventory) {
		return cfg.MaxNetInventory
	}
	return cfg.MaxLongInventory
}

func effectiveMaxShort(cfg config.Config) float64 {
	if cfg.MaxNetInventory > 0 {
		netShort := -cfg.MaxNetInventory
		if cfg.MaxShortInventory == 0 || netShort > cfg.MaxShortInventory {
			return netShort
		}
	}
	return cfg.MaxShortInventory
}

func inventorySkew(inventory, maxLong, maxShort, maxSkewBPS float64) float64 {
	if maxSkewBPS == 0 {
		return 0
	}
	limit := math.Max(math.Abs(maxLong), math.Abs(maxShort))
	if limit == 0 {
		return 0
	}
	ratio := inventory / limit
	if ratio > 1 {
		ratio = 1
	}
	if ratio < -1 {
		ratio = -1
	}
	return ratio * maxSkewBPS
}

func roundDown(value, step float64) float64 {
	if step <= 0 {
		return value
	}
	return math.Floor((value/step)+1e-9) * step
}

func roundUp(value, step float64) float64 {
	if step <= 0 {
		return value
	}
	return math.Ceil((value/step)-1e-9) * step
}

func isFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
