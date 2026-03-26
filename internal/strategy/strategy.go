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
	ReferencePrice float64
	Bid            *Quote
	Ask            *Quote
	SkewBPS        float64
}

func ComputeReferencePrice(snapshot state.Snapshot) float64 {
	if snapshot.BestBid > 0 && snapshot.BestAsk > 0 {
		return (snapshot.BestBid + snapshot.BestAsk) / 2
	}
	if len(snapshot.RecentTrades) > 0 {
		return snapshot.RecentTrades[0].Price
	}
	return 0
}

func BuildQuotes(cfg config.Config, spec exchange.MarketSpec, snapshot state.Snapshot) (Result, error) {
	ref := ComputeReferencePrice(snapshot)
	result := Result{ReferencePrice: ref}
	if ref <= 0 {
		return result, nil
	}

	inventory := snapshot.Inventory(spec.BaseAsset)
	skewBPS := inventorySkew(inventory, cfg.MaxLongInventory, cfg.MaxShortInventory, cfg.InventorySkewBPS)
	halfSpread := cfg.HalfSpreadBPS / 10000.0
	skew := skewBPS / 10000.0

	bidPrice := roundDown(ref*(1-halfSpread-skew), spec.TickSize)
	askPrice := roundUp(ref*(1+halfSpread-skew), spec.TickSize)
	if bidPrice <= 0 || askPrice <= 0 || bidPrice >= askPrice || !isFinite(bidPrice) || !isFinite(askPrice) {
		return result, fmt.Errorf("calculated invalid quote prices")
	}

	baseAvailable := snapshot.Position(spec.BaseAsset).Available
	quoteAvailable := snapshot.Position(spec.QuoteAsset).Available

	maxBidSize := quoteAvailable / bidPrice
	maxAskSize := baseAvailable
	bidSize := roundDown(minFloat(cfg.OrderSize, maxBidSize), spec.SizeStep)
	askSize := roundDown(minFloat(cfg.OrderSize, maxAskSize), spec.SizeStep)

	if bidSize >= spec.MinSize && inventory+bidSize <= cfg.MaxLongInventory {
		result.Bid = &Quote{Side: exchange.SideBuy, Price: bidPrice, Size: bidSize}
	}
	if askSize >= spec.MinSize && inventory-askSize >= cfg.MaxShortInventory {
		result.Ask = &Quote{Side: exchange.SideSell, Price: askPrice, Size: askSize}
	}
	result.SkewBPS = skewBPS
	return result, nil
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
