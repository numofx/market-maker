package strategy

import (
	"math"
	"testing"
	"time"

	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/state"
)

func perpSpec() exchange.MarketSpec {
	return exchange.MarketSpec{
		Kind:           exchange.MarketKindPerp,
		Symbol:         "USDCcNGN-PERP",
		BaseAsset:      "USDC",
		QuoteAsset:     "cNGN",
		OrderEntrySpec: "usdc_cngn_perp_v1",
		TickSize:       0.000000000000000001,
		SizeStep:       0.000001,
		MinSize:        0.000001,
		TakerFeeBps:    25,
	}
}

// perpSnapshot: cash in USD, position as a signed UI notional in USD (long positive).
func perpSnapshot(cash, position float64, perp state.PerpSnapshot) state.Snapshot {
	return state.Snapshot{
		Market:                "USDCcNGN-PERP",
		InventoryByAsset:      map[string]float64{"USDC": position, "cNGN": cash},
		Positions:             map[string]state.AssetPosition{"USDC": {Total: position, Available: position}, "cNGN": {Total: cash, Available: cash}},
		LastMarketDataRefresh: time.Now().UTC(),
		LastBalanceRefresh:    time.Now().UTC(),
		Perp:                  &perp,
	}
}

func livePerp() state.PerpSnapshot {
	return state.PerpSnapshot{Reference: 1374, ReferenceSource: "index", IndexPrice: 1374, MarkPrice: 1374, TradingEnabled: true, SideRoomUSD: 1_000_000, MaxLeverage: 3}
}

func TestPerpCapacity(t *testing.T) {
	cfg := baseCfg()
	cfg.PerpMaxLeverage = 1.5
	cases := []struct {
		name             string
		perp             state.PerpSnapshot
		cash, position   float64
		wantBid, wantAsk float64
	}{
		// Flat with $10k: 1.5x each way.
		{"flat", livePerp(), 10_000, 0, 15_000, 15_000},
		// Long $6k: can add $9k more long, or sell $6k to flat and $15k beyond.
		{"long", livePerp(), 10_000, 6_000, 9_000, 21_000},
		// The SRM's ceiling wins when it is lower than the bot's own.
		{"srm lower", state.PerpSnapshot{TradingEnabled: true, SideRoomUSD: 1_000_000, MaxLeverage: 1}, 10_000, 0, 10_000, 10_000},
		// OI room binds what opens a position, never what closes one: short $4k with $1k of room
		// can buy the $4k back plus $1k.
		{"oi room", state.PerpSnapshot{TradingEnabled: true, SideRoomUSD: 1_000, MaxLeverage: 3}, 10_000, -4_000, 5_000, 1_000},
		// Negative cash backs nothing.
		{"no cash", livePerp(), -50, 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bid, ask := perpCapacity(cfg, tc.perp, tc.cash, tc.position)
			if math.Abs(bid-tc.wantBid) > 1e-9 || math.Abs(ask-tc.wantAsk) > 1e-9 {
				t.Fatalf("capacity = %v/%v, want %v/%v", bid, ask, tc.wantBid, tc.wantAsk)
			}
		})
	}
}

func TestPerpQuotesAroundTheReferenceWithinTheLeverageCap(t *testing.T) {
	cfg := baseCfg()
	cfg.PerpMaxLeverage = 1.5
	cfg.OrderSize = 50_000 // larger than the cap allows: the cap must bind
	// On the perp the inventory limits are USD of position, like the sizes.
	cfg.MaxLongInventory, cfg.MaxShortInventory = 100_000, -100_000
	res, err := BuildQuotes(cfg, perpSpec(), perpSnapshot(10_000, 0, livePerp()))
	if err != nil {
		t.Fatalf("BuildQuotes: %v", err)
	}
	if res.Bid == nil || res.Ask == nil {
		t.Fatalf("want both sides, got bid=%v ask=%v (%+v / %+v)", res.Bid, res.Ask, res.BidSuppression, res.AskSuppression)
	}
	if res.Bid.Price >= 1374 || res.Ask.Price <= 1374 {
		t.Fatalf("quotes %v / %v do not straddle the 1374 reference", res.Bid.Price, res.Ask.Price)
	}
	if res.Bid.Size > 15_000+1e-6 || res.Ask.Size > 15_000+1e-6 {
		t.Fatalf("sizes %v / %v exceed 1.5x of $10k", res.Bid.Size, res.Ask.Size)
	}
}

func TestPerpQuotesNothingUntilTradingIsEnabled(t *testing.T) {
	perp := livePerp()
	perp.TradingEnabled = false
	res, err := BuildQuotes(baseCfg(), perpSpec(), perpSnapshot(10_000, 0, perp))
	if err != nil {
		t.Fatalf("BuildQuotes: %v", err)
	}
	if res.Bid != nil || res.Ask != nil || len(res.Bids) != 0 || len(res.Asks) != 0 {
		t.Fatalf("quoted a closed market: %+v", res)
	}
	if res.BidSuppression == nil || res.BidSuppression.Reason != "perp_trading_disabled" {
		t.Fatalf("bid suppression = %+v, want perp_trading_disabled", res.BidSuppression)
	}
}

// The launch: the enable gate needs a two-sided book before it opens the market, so the bot can be
// told to rest quotes while closed. Nothing fills until the vault opens it.
func TestPerpQuotesWhileClosedWhenTheLaunchAsksForIt(t *testing.T) {
	cfg := baseCfg()
	cfg.PerpMaxLeverage = 1.5
	cfg.PerpQuoteWhileClosed = true
	perp := livePerp()
	perp.TradingEnabled = false
	res, err := BuildQuotes(cfg, perpSpec(), perpSnapshot(10_000, 0, perp))
	if err != nil {
		t.Fatalf("BuildQuotes: %v", err)
	}
	if res.Bid == nil || res.Ask == nil {
		t.Fatalf("want a two-sided quote for the enable gate, got %+v / %+v", res.BidSuppression, res.AskSuppression)
	}
}

func TestPerpReferenceComesFromTheLoader(t *testing.T) {
	perp := livePerp()
	perp.Reference, perp.ReferenceSource = 1360.26, "book_clamped"
	price, source := ComputeReferencePrice(perpSnapshot(1, 0, perp))
	if price != 1360.26 || source != "book_clamped" {
		t.Fatalf("reference %v/%s", price, source)
	}
}
