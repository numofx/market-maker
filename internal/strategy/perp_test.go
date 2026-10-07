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
		BaseAsset:      "cNGN",
		QuoteAsset:     "USDC",
		OrderEntrySpec: exchange.SpecCNGNUSDCPerp,
		TickSize:       0.000000000000000001,
		SizeStep:       1,
		MinSize:        1,
		TakerFeeBps:    25,
	}
}

// The index the venue published at the perp's launch, as the bot reads it: USDC per cNGN.
const perpIndex = 1 / 1374.0

// perpSnapshot: cash in USDC, position as the engine's signed cNGN amount (long cNGN positive).
func perpSnapshot(cash, positionNGN float64, perp state.PerpSnapshot) state.Snapshot {
	return state.Snapshot{
		Market:                "USDCcNGN-PERP",
		InventoryByAsset:      map[string]float64{"cNGN": positionNGN, "USDC": cash},
		Positions:             map[string]state.AssetPosition{"cNGN": {Total: positionNGN, Available: positionNGN}, "USDC": {Total: cash, Available: cash}},
		LastMarketDataRefresh: time.Now().UTC(),
		LastBalanceRefresh:    time.Now().UTC(),
		Perp:                  &perp,
	}
}

func livePerp() state.PerpSnapshot {
	return state.PerpSnapshot{
		Reference: perpIndex, ReferenceSource: "index", IndexPrice: perpIndex, MarkPrice: perpIndex,
		TradingEnabled: true, SideRoomNGN: 1_374_000_000, SideRoomUSD: 1_000_000, MaxLeverage: 3,
	}
}

// Capacity is a USDC leverage bound converted to cNGN at the price. At 0.001 USDC per cNGN the
// numbers are round: $10k of cash at 1.5x is $15k, or 15,000,000 cNGN, a side.
func TestPerpCapacity(t *testing.T) {
	cfg := baseCfg()
	cfg.PerpMaxLeverage = 1.5
	const px = 0.001
	cases := []struct {
		name             string
		perp             state.PerpSnapshot
		cash, position   float64
		wantBid, wantAsk float64
	}{
		// Flat with $10k: 1.5x each way.
		{"flat", livePerp(), 10_000, 0, 15_000_000, 15_000_000},
		// Long 6M cNGN ($6k): can add $9k more long, or sell $6k to flat and $15k beyond.
		{"long", livePerp(), 10_000, 6_000_000, 9_000_000, 21_000_000},
		// The SRM's ceiling wins when it is lower than the bot's own.
		{"srm lower", state.PerpSnapshot{TradingEnabled: true, SideRoomNGN: 1e12, MaxLeverage: 1}, 10_000, 0, 10_000_000, 10_000_000},
		// OI room binds what opens a position, never what closes one: short 4M cNGN with 1M cNGN
		// of room can buy the 4M back plus 1M.
		{"oi room", state.PerpSnapshot{TradingEnabled: true, SideRoomNGN: 1_000_000, MaxLeverage: 3}, 10_000, -4_000_000, 5_000_000, 1_000_000},
		// Negative cash backs nothing.
		{"no cash", livePerp(), -50, 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bid, ask := perpCapacity(cfg, tc.perp, tc.cash, tc.position, px)
			if math.Abs(bid-tc.wantBid) > 1e-6 || math.Abs(ask-tc.wantAsk) > 1e-6 {
				t.Fatalf("capacity = %v/%v, want %v/%v", bid, ask, tc.wantBid, tc.wantAsk)
			}
		})
	}
	if b, a := perpCapacity(cfg, livePerp(), 10_000, 0, 0); b != 0 || a != 0 {
		t.Fatalf("no price, no capacity: got %v/%v", b, a)
	}
}

func TestPerpQuotesAroundTheReferenceWithinTheLeverageCap(t *testing.T) {
	cfg := baseCfg()
	cfg.PerpMaxLeverage = 1.5
	cfg.OrderSize = 50_000 // USDC, larger than the cap allows: the cap must bind
	// On the perp the inventory limits are USDC of position, like the sizes.
	cfg.MaxLongInventory, cfg.MaxShortInventory = 100_000, -100_000
	res, err := BuildQuotes(cfg, perpSpec(), perpSnapshot(10_000, 0, livePerp()))
	if err != nil {
		t.Fatalf("BuildQuotes: %v", err)
	}
	if res.Bid == nil || res.Ask == nil {
		t.Fatalf("want both sides, got bid=%v ask=%v (%+v / %+v)", res.Bid, res.Ask, res.BidSuppression, res.AskSuppression)
	}
	if res.Bid.Price >= perpIndex || res.Ask.Price <= perpIndex {
		t.Fatalf("quotes %v / %v do not straddle the %v reference", res.Bid.Price, res.Ask.Price, perpIndex)
	}
	// 1.5x of $10k is $15k of cNGN at the reference, and whole cNGN.
	capNGN := 15_000 / perpIndex
	for _, q := range []*Quote{res.Bid, res.Ask} {
		if q.Size > capNGN+1 || q.Size != math.Floor(q.Size) || q.Size*perpIndex < 14_990 {
			t.Fatalf("size %v cNGN (%.2f USDC), want ~$15k of whole cNGN", q.Size, q.Size*perpIndex)
		}
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

func TestPerpQuotesWhileClosedIgnoreTheZeroCapRoom(t *testing.T) {
	// A closed perp has totalPositionCap 0, so its room is 0. The flag must still rest quotes
	// (the enable gate needs them); without the flag, or once the market is open, room binds.
	cfg := baseCfg()
	cfg.PerpMaxLeverage = 1.5
	cfg.PerpQuoteWhileClosed = true
	closed := livePerp()
	closed.TradingEnabled, closed.SideRoomNGN, closed.SideRoomUSD = false, 0, 0
	res, err := BuildQuotes(cfg, perpSpec(), perpSnapshot(4_000, 0, closed))
	if err != nil {
		t.Fatalf("BuildQuotes: %v", err)
	}
	if res.Bid == nil || res.Ask == nil {
		t.Fatalf("want a two-sided quote against a closed cap-0 perp, got %+v / %+v", res.BidSuppression, res.AskSuppression)
	}
	const px = 0.001
	maxBid, maxAsk := perpCapacity(cfg, closed, 4_000, 0, px)
	if maxBid != 6_000_000 || maxAsk != 6_000_000 {
		t.Fatalf("closed + flag: capacity is the leverage bound, got %v / %v", maxBid, maxAsk)
	}
	cfg.PerpQuoteWhileClosed = false
	if b, a := perpCapacity(cfg, closed, 4_000, 0, px); b != 0 || a != 0 {
		t.Fatalf("closed without the flag: room 0 binds, got %v / %v", b, a)
	}
	cfg.PerpQuoteWhileClosed = true
	open := closed
	open.TradingEnabled = true
	if b, a := perpCapacity(cfg, open, 4_000, 0, px); b != 0 || a != 0 {
		t.Fatalf("open with room 0: room binds regardless of the flag, got %v / %v", b, a)
	}
}

func TestPerpReferenceComesFromTheLoader(t *testing.T) {
	perp := livePerp()
	perp.Reference, perp.ReferenceSource = 0.000735, "book_clamped"
	price, source := ComputeReferencePrice(perpSnapshot(1, 0, perp))
	if price != 0.000735 || source != "book_clamped" {
		t.Fatalf("reference %v/%s", price, source)
	}
}
