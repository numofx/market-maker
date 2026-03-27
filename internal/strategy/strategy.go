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

type Result struct {
	ReferencePrice       float64
	ReferenceSource      string
	LocalReferencePrice  float64
	LocalReferenceSource string
	AnchorPrice          float64
	Bid                  *Quote
	Ask                  *Quote
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

	baseAvailable := snapshot.Position(spec.BaseAsset).Available
	quoteAvailable := snapshot.Position(spec.QuoteAsset).Available
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
	}
	if askSize >= spec.MinSize && inventory-askSize >= effectiveMaxShort(cfg) {
		result.Ask = &Quote{Side: exchange.SideSell, Price: askPrice, Size: askSize}
	}

	switch cfg.OperatorMode {
	case config.ModePause, config.ModeDryRunHealth:
		result.Bid = nil
		result.Ask = nil
	case config.ModeBidOnly:
		result.Ask = nil
	case config.ModeAskOnly:
		result.Bid = nil
	}
	result.SkewBPS = skewBPS
	return result, nil
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
