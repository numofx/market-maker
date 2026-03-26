package risk

import (
	"fmt"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/state"
)

type Decision struct {
	Halt   bool
	Reason string
}

func Evaluate(cfg config.Config, spec exchange.MarketSpec, snapshot state.Snapshot) Decision {
	if snapshot.ReferencePrice <= 0 {
		return Decision{Halt: true, Reason: "reference price unavailable"}
	}

	inventory := snapshot.Inventory(spec.BaseAsset)
	if inventory > cfg.MaxLongInventory {
		return Decision{Halt: true, Reason: fmt.Sprintf("inventory %.6f exceeds max long %.6f", inventory, cfg.MaxLongInventory)}
	}
	if inventory < cfg.MaxShortInventory {
		return Decision{Halt: true, Reason: fmt.Sprintf("inventory %.6f exceeds max short %.6f", inventory, cfg.MaxShortInventory)}
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
