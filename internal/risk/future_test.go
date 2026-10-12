package risk

import (
	"strings"
	"testing"
	"time"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/state"
)

func engineFuture() exchange.MarketSpec {
	return exchange.MarketSpec{Symbol: "USDC-cNGN-31DEC26", Kind: exchange.MarketKindFuture, BaseAsset: "USDC", QuoteAsset: "cNGN",
		ContractSizeUSDC: 1000, MarginManager: "0x00000000000000000000000000000000000000e9"}
}

func snapshotWith(orders []exchange.Order, position float64) state.Snapshot {
	return state.Snapshot{ReferencePrice: 1400, LastMarketDataRefresh: time.Now().UTC(), LastBalanceRefresh: time.Now().UTC(),
		OpenOrders:       orders,
		InventoryByAsset: map[string]float64{"USDC": position},
		Positions:        map[string]state.AssetPosition{"USDC": {Total: position}, "cNGN": {Total: 0}}}
}

// MM_MAX_NOTIONAL_PER_SIDE is USDC: an order of 5 contracts is 5,000 USDC, not 5 x 1,400 = 7,000.
func TestOpenOrderNotionalOnTheEngineFutureIsContractsTimesContractSize(t *testing.T) {
	cfg := config.Config{MaxNotionalPerSide: 6000, MaxLongInventory: 100, MaxShortInventory: -100}
	five := []exchange.Order{{Side: exchange.SideBuy, Price: 1400, Size: 5}}
	if d := Evaluate(cfg, engineFuture(), snapshotWith(five, 0)); d.Halt {
		t.Fatalf("5 contracts is 5,000 USDC, under 6,000: %s", d.Reason)
	}
	seven := []exchange.Order{{Side: exchange.SideBuy, Price: 1400, Size: 7}}
	if d := Evaluate(cfg, engineFuture(), snapshotWith(seven, 0)); !d.Halt || !strings.Contains(d.Reason, "notional") {
		t.Fatalf("7 contracts is 7,000 USDC, over 6,000: %+v", d)
	}
}

// The engine future's base "balance" is a signed position in contracts, not a holding: a short position
// is not a base balance below MM_MIN_BASE_BALANCE.
func TestAShortEngineFuturePositionIsNotALowBaseBalance(t *testing.T) {
	cfg := config.Config{MinBaseBalance: 0.001, MaxLongInventory: 100, MaxShortInventory: -100}
	if d := Evaluate(cfg, engineFuture(), snapshotWith(nil, -3)); d.Halt {
		t.Fatalf("a short position halts nothing: %s", d.Reason)
	}
}
