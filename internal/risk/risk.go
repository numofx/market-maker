package risk

import (
	"fmt"
	"time"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/state"
)

type Decision struct {
	Halt   bool
	Reason string
}

func Evaluate(cfg config.Config, spec exchange.MarketSpec, snapshot state.Snapshot) Decision {
	now := time.Now().UTC()
	if snapshot.ReferencePrice <= 0 {
		return Decision{Halt: true, Reason: "reference price unavailable"}
	}
	if cfg.StaleMarketDataTimeout > 0 && !snapshot.LastMarketDataRefresh.IsZero() && now.Sub(snapshot.LastMarketDataRefresh) > cfg.StaleMarketDataTimeout {
		return Decision{Halt: true, Reason: "market data stale"}
	}
	if cfg.StaleBalanceTimeout > 0 && !snapshot.LastBalanceRefresh.IsZero() && now.Sub(snapshot.LastBalanceRefresh) > cfg.StaleBalanceTimeout {
		return Decision{Halt: true, Reason: "balances stale"}
	}
	if cfg.StaleAnchorTimeout > 0 && snapshot.AnchorSource != "" && snapshot.AnchorSource != "none" && !snapshot.LastAnchorRefresh.IsZero() && now.Sub(snapshot.LastAnchorRefresh) > cfg.StaleAnchorTimeout {
		return Decision{Halt: true, Reason: "anchor data stale"}
	}
	if cfg.MaxQuoteAge > 0 && len(snapshot.OpenOrders) > 0 && !snapshot.LastQuoteUpdate.IsZero() && now.Sub(snapshot.LastQuoteUpdate) > cfg.MaxQuoteAge {
		return Decision{Halt: true, Reason: "quote age exceeded"}
	}
	if cfg.MaxAnchorDeviationBPS > 0 && snapshot.AnchorPrice > 0 && snapshot.LocalReferencePrice > 0 && snapshot.AnchorDeviationBPS > cfg.MaxAnchorDeviationBPS {
		return Decision{Halt: true, Reason: "anchor deviation exceeded"}
	}

	inventory := snapshot.Inventory(spec.BaseAsset)
	maxLong := cfg.MaxLongInventory
	maxShort := cfg.MaxShortInventory
	if cfg.MaxNetInventory > 0 {
		if maxLong == 0 || cfg.MaxNetInventory < maxLong {
			maxLong = cfg.MaxNetInventory
		}
		if maxShort == 0 || -cfg.MaxNetInventory > maxShort {
			maxShort = -cfg.MaxNetInventory
		}
	}
	if inventory > maxLong {
		return Decision{Halt: true, Reason: fmt.Sprintf("inventory %.6f exceeds max long %.6f", inventory, maxLong)}
	}
	if inventory < maxShort {
		return Decision{Halt: true, Reason: fmt.Sprintf("inventory %.6f exceeds max short %.6f", inventory, maxShort)}
	}
	if cfg.MaxNotionalPerSide > 0 {
		for _, order := range snapshot.OpenOrders {
			notional := order.Price * order.Size
			if notional > cfg.MaxNotionalPerSide {
				return Decision{Halt: true, Reason: "open order notional exceeds limit"}
			}
		}
	}

	basePosition := snapshot.Position(spec.BaseAsset)
	if basePosition.Available < cfg.MinBaseBalance {
		return Decision{Halt: true, Reason: fmt.Sprintf("available base balance below threshold for %s", spec.BaseAsset)}
	}
	quotePosition := snapshot.Position(spec.QuoteAsset)
	if quotePosition.Available < cfg.MinQuoteBalance {
		return Decision{Halt: true, Reason: fmt.Sprintf("available quote balance below threshold for %s", spec.QuoteAsset)}
	}
	return Decision{}
}
