package strategy

import (
	"math"
	"time"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/state"
)

// The cNGN future under the MarginEngine (exchange.MarketSpec.IsMarginEngineFuture). Quoted in cNGN per
// USDC; one contract is spec.ContractSizeUSDC of notional; margined by the engine's IM, not by cash.

const (
	reasonFutureStateMissing = "future_state_unavailable"
	reasonFutureCutoff       = "future_quoting_cutoff"
	reasonFutureSettled      = "future_settled"
	reasonOutsideFillBand    = "outside_fill_band"
)

// marginEngineFutureCapacity is how many contracts each side may quote. Opening risk is bounded by the
// usable IM surplus (MM_FUTURE_IM_USAGE of it) over the IM a contract adds; the contracts that only
// close the current position need none. The engine checks IM per fill, not per resting order, so each
// side may use the whole headroom: only one of them fills at a time.
func marginEngineFutureCapacity(cfg config.Config, future *exchange.FutureState, inventory float64) (maxBid, maxAsk float64) {
	if future == nil || future.PerContractIM <= 0 {
		return 0, 0
	}
	opening := math.Max(0, future.IMSurplus) * cfg.FutureIMUsage / future.PerContractIM
	maxBid = opening + math.Max(0, -inventory)
	maxAsk = opening + math.Max(0, inventory)
	return maxBid, maxAsk
}

// marginEngineFutureNotionalCap is MM_MAX_NOTIONAL_PER_SIDE (USDC) in contracts.
func marginEngineFutureNotionalCap(cfg config.Config, spec exchange.MarketSpec) float64 {
	if cfg.MaxNotionalPerSide <= 0 || spec.ContractSizeUSDC <= 0 {
		return 0
	}
	return cfg.MaxNotionalPerSide / spec.ContractSizeUSDC
}

// applyMarginEngineFuture takes the future off the book from its quoting cutoff (the earlier of the
// fixing window's start and last trade, less MM_FUTURE_CUTOFF_BEFORE_FIX_SECONDS) and keeps every level
// inside the fill band, MM_FUTURE_BAND_BUFFER_BPS in from each edge. A level outside the band could
// only rest: the venue refuses the order and the chain the fill. A bid above the band moves down to
// it and an ask below it moves up, which only widens the quote; a bid below or an ask above is dropped.
func applyMarginEngineFuture(cfg config.Config, spec exchange.MarketSpec, snapshot state.Snapshot, result *Result, now time.Time) {
	off := func(reason string) {
		result.Bid, result.Ask, result.Bids, result.Asks = nil, nil, nil, nil
		result.BidSuppression = &Suppression{Side: exchange.SideBuy, Reason: reason, Market: spec.Symbol}
		result.AskSuppression = &Suppression{Side: exchange.SideSell, Reason: reason, Market: spec.Symbol}
	}
	future := snapshot.Future
	switch {
	case future == nil:
		off(reasonFutureStateMissing)
		return
	case future.Settled:
		off(reasonFutureSettled)
		return
	case !now.Before(future.QuotingCutoff(time.Duration(cfg.FutureCutoffBeforeFixSeconds) * time.Second)):
		off(reasonFutureCutoff)
		return
	}

	buffer := cfg.FutureBandBufferBPS / 10_000
	low := roundUp(future.BandLow*(1+buffer), spec.TickSize)
	high := roundDown(future.BandHigh*(1-buffer), spec.TickSize)
	if !(low > 0) || high < low {
		off(reasonOutsideFillBand)
		return
	}
	result.Bids = clampLevels(result.Bids, exchange.SideBuy, low, high)
	result.Asks = clampLevels(result.Asks, exchange.SideSell, low, high)
	result.Bid, result.Ask = firstLevel(result.Bids), firstLevel(result.Asks)
	if result.Bid == nil && result.BidSuppression == nil {
		result.BidSuppression = &Suppression{Side: exchange.SideBuy, Reason: reasonOutsideFillBand, Market: spec.Symbol}
	}
	if result.Ask == nil && result.AskSuppression == nil {
		result.AskSuppression = &Suppression{Side: exchange.SideSell, Reason: reasonOutsideFillBand, Market: spec.Symbol}
	}
}

// clampLevels moves levels into [low, high] in the safe direction only, and drops the rest. Two levels
// clamped to the same price become one: the syncer pairs resting orders to targets by price.
func clampLevels(levels []Quote, side exchange.Side, low, high float64) []Quote {
	out := make([]Quote, 0, len(levels))
	seen := map[float64]bool{}
	for _, level := range levels {
		if side == exchange.SideBuy {
			if level.Price < low {
				continue
			}
			if level.Price > high {
				level.Price = high
			}
		} else {
			if level.Price > high {
				continue
			}
			if level.Price < low {
				level.Price = low
			}
		}
		if seen[level.Price] {
			continue
		}
		seen[level.Price] = true
		out = append(out, level)
	}
	return out
}

func firstLevel(levels []Quote) *Quote {
	if len(levels) == 0 {
		return nil
	}
	first := levels[0]
	return &first
}
