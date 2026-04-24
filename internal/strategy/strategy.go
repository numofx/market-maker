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
	BidSuppression       *Suppression
	AskSuppression       *Suppression
	SkewBPS              float64
}

func ComputeReferencePrice(snapshot state.Snapshot) (float64, string) {
	if snapshot.Market == "USDCcNGN-SPOT" {
		localRef, localSource := ComputeLocalReference(snapshot)
		if localRef > 0 {
			return localRef, localSource
		}
		if snapshot.ExternalAnchorPrice > 0 {
			return snapshot.ExternalAnchorPrice, "external"
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
	if len(snapshot.RecentTrades) > 0 {
		return snapshot.RecentTrades[0].Price, "trade"
	}
	return 0, "none"
}

func BuildQuotes(cfg config.Config, spec exchange.MarketSpec, snapshot state.Snapshot) (Result, error) {
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
		result.BidSuppression = baseSuppression(cfg, spec, snapshot, exchange.SideBuy, "no_anchor", 0, 0, 0, 0, 0, 0, 0, false)
		result.AskSuppression = baseSuppression(cfg, spec, snapshot, exchange.SideSell, "no_anchor", 0, 0, 0, 0, 0, 0, 0, false)
		return result, nil
	}

	inventory := snapshot.Inventory(spec.BaseAsset)
	skewBPS := inventorySkew(inventory, cfg.MaxLongInventory, cfg.MaxShortInventory, cfg.InventorySkewBPS)
	halfSpreadBPS := cfg.HalfSpreadBPS
	orderSize := cfg.OrderSize
	if snapshot.Market == "USDCcNGN-SPOT" && refSource == "external" {
		halfSpreadBPS *= cfg.USDCCNGNSpotExternalAnchor.SpreadMultiplier
		orderSize *= cfg.USDCCNGNSpotExternalAnchor.SizeMultiplier
	}
	halfSpread := halfSpreadBPS / 10000.0
	skew := skewBPS / 10000.0

	bidPrice := roundDown(ref*(1-halfSpread-skew), spec.TickSize)
	askPrice := roundUp(ref*(1+halfSpread-skew), spec.TickSize)
	if bidPrice <= 0 || askPrice <= 0 || bidPrice >= askPrice || !isFinite(bidPrice) || !isFinite(askPrice) {
		return result, fmt.Errorf("calculated invalid quote prices")
	}

	basePosition := snapshot.Position(spec.BaseAsset)
	quotePosition := snapshot.Position(spec.QuoteAsset)
	baseAvailable := basePosition.Available
	quoteAvailable := quotePosition.Available
	reusableBase, reusableQuote := reusableCapacity(spec, snapshot.OpenOrders)
	baseAvailable += reusableBase
	quoteAvailable += reusableQuote

	maxBidSize := quoteAvailable / bidPrice
	maxAskSize := baseAvailable
	if cfg.MaxNotionalPerSide > 0 {
		maxBidSize = minFloat(maxBidSize, cfg.MaxNotionalPerSide/bidPrice)
		maxAskSize = minFloat(maxAskSize, cfg.MaxNotionalPerSide/askPrice)
	}
	bidSize := roundDown(minFloat(orderSize, maxBidSize), spec.SizeStep)
	askSize := roundDown(minFloat(orderSize, maxAskSize), spec.SizeStep)

	if bidSize >= spec.MinSize && inventory+bidSize <= effectiveMaxLong(cfg) {
		result.Bid = &Quote{Side: exchange.SideBuy, Price: bidPrice, Size: bidSize}
	} else {
		result.BidSuppression = baseSuppression(cfg, spec, snapshot, exchange.SideBuy, bidSuppressionReason(orderSize, bidSize, spec.MinSize, maxBidSize, quoteAvailable, inventory, effectiveMaxLong(cfg)), spec.MinSize*bidPrice, quotePosition.Total, quotePosition.Reserved, quoteAvailable, orderSize, bidSize, bidPrice, false)
	}
	if askSize >= spec.MinSize && inventory-askSize >= effectiveMaxShort(cfg) {
		result.Ask = &Quote{Side: exchange.SideSell, Price: askPrice, Size: askSize}
	} else {
		result.AskSuppression = baseSuppression(cfg, spec, snapshot, exchange.SideSell, askSuppressionReason(orderSize, askSize, spec.MinSize, maxAskSize, baseAvailable, basePosition.Total, inventory, effectiveMaxShort(cfg)), spec.MinSize, basePosition.Total, basePosition.Reserved, baseAvailable, orderSize, askSize, askPrice, false)
	}

	switch cfg.OperatorMode {
	case config.ModePause, config.ModeDryRunHealth:
		result.Bid = nil
		result.Ask = nil
		result.BidSuppression = baseSuppression(cfg, spec, snapshot, exchange.SideBuy, "operator_halted", spec.MinSize*bidPrice, quotePosition.Total, quotePosition.Reserved, quoteAvailable, orderSize, bidSize, bidPrice, false)
		result.AskSuppression = baseSuppression(cfg, spec, snapshot, exchange.SideSell, "operator_halted", spec.MinSize, basePosition.Total, basePosition.Reserved, baseAvailable, orderSize, askSize, askPrice, false)
	case config.ModeBidOnly:
		result.Ask = nil
		result.AskSuppression = baseSuppression(cfg, spec, snapshot, exchange.SideSell, "operator_halted", spec.MinSize, basePosition.Total, basePosition.Reserved, baseAvailable, orderSize, askSize, askPrice, false)
	case config.ModeAskOnly:
		result.Bid = nil
		result.BidSuppression = baseSuppression(cfg, spec, snapshot, exchange.SideBuy, "operator_halted", spec.MinSize*bidPrice, quotePosition.Total, quotePosition.Reserved, quoteAvailable, orderSize, bidSize, bidPrice, false)
	}
	result.SkewBPS = skewBPS
	return result, nil
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

func baseSuppression(cfg config.Config, spec exchange.MarketSpec, snapshot state.Snapshot, side exchange.Side, reason string, requiredCapacity, totalCapacity, reservedCapacity, availableCapacity, effectiveOrderSize, candidateSize, price float64, dependencyStale bool) *Suppression {
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
		MaxInventory:         maxInventoryForSide(cfg, side),
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

func maxInventoryForSide(cfg config.Config, side exchange.Side) float64 {
	if side == exchange.SideBuy {
		return effectiveMaxLong(cfg)
	}
	return effectiveMaxShort(cfg)
}

func reusableCapacity(spec exchange.MarketSpec, orders []exchange.Order) (base, quote float64) {
	for _, order := range orders {
		switch order.Side {
		case exchange.SideBuy:
			quote += order.Size * order.Price
		case exchange.SideSell:
			base += order.Size
		}
	}
	return base, quote
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
