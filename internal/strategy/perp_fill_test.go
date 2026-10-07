package strategy

import (
	"math"
	"testing"

	"github.com/numofx/market-maker/internal/exchange"
)

// Fills each side of the perp ladder, applies the fill to the on-chain cNGN position, reads the
// position back the way GetBalances does, requotes, and checks direction, sign and lean. The bot's
// orientation is the engine's: a bid buys cNGN and leaves the position long cNGN (positive), and a
// long must lean DOWN in USDC per cNGN -- sell cNGN cheaper, bid for it lower.
//
// A bot that had a sign wrong would lean INTO its inventory: after being lifted it would quote
// higher and keep buying.
func TestPerpFillEachSide(t *testing.T) {
	cfg := baseCfg()
	cfg.PerpMaxLeverage = 1.5
	cfg.OrderSize = 1_000
	cfg.InventorySkewBPS = 50
	cfg.MaxLongInventory, cfg.MaxShortInventory = 10_000, -10_000

	spec := perpSpec()
	spec.AssetAddress, spec.SubID, spec.QuoteAddress = "0xperp", "0", "0xcash"
	spec.Perp = &exchange.PerpState{IndexPrice: perpIndex}
	const cash = 20_000.0

	quote := func(t *testing.T, positionNGN float64) Result {
		t.Helper()
		res, err := BuildQuotes(cfg, spec, perpSnapshot(cash, positionNGN, livePerp()))
		if err != nil || res.Bid == nil || res.Ask == nil {
			t.Fatalf("BuildQuotes at position %v: %v (bid %v ask %v)", positionNGN, err, res.Bid, res.Ask)
		}
		return res
	}
	// What GetBalances reports after the fill moves the cNGN position by delta.
	positionAfter := func(t *testing.T, delta float64) float64 {
		t.Helper()
		balances, err := exchange.PerpBalances(spec, map[string]float64{"0xperp|0": delta, "0xcash|0": cash})
		if err != nil {
			t.Fatalf("PerpBalances: %v", err)
		}
		return balances[0].Total
	}

	flat := quote(t, 0)
	if flat.Bid.Side != exchange.SideBuy || flat.Ask.Side != exchange.SideSell {
		t.Fatalf("sides %s/%s: a bid is an engine buy of cNGN", flat.Bid.Side, flat.Ask.Side)
	}
	if want := math.Floor(1_000 / perpIndex); flat.Bid.Size != want {
		t.Fatalf("rung %v cNGN, want %v ($1,000 at the index)", flat.Bid.Size, want)
	}

	t.Run("bid filled: the bot is long cNGN and leans down to sell it", func(t *testing.T) {
		position := positionAfter(t, +flat.Bid.Size)
		if position <= 0 || position != flat.Bid.Size {
			t.Fatalf("position %v cNGN after a bid fill, want +%v", position, flat.Bid.Size)
		}
		after := quote(t, position)
		if !(after.Bid.Price < flat.Bid.Price && after.Ask.Price < flat.Ask.Price) {
			t.Fatalf("quotes %v/%v did not lean down from %v/%v", after.Bid.Price, after.Ask.Price, flat.Bid.Price, flat.Ask.Price)
		}
		if after.SkewBPS <= 0 {
			t.Fatalf("skew %v bps, want positive for a long", after.SkewBPS)
		}
	})

	t.Run("ask filled: the bot is short cNGN and leans up to buy it back", func(t *testing.T) {
		position := positionAfter(t, -flat.Ask.Size)
		if position >= 0 {
			t.Fatalf("position %v cNGN after an ask fill, want negative", position)
		}
		after := quote(t, position)
		if !(after.Bid.Price > flat.Bid.Price && after.Ask.Price > flat.Ask.Price) {
			t.Fatalf("quotes %v/%v did not lean up from %v/%v", after.Bid.Price, after.Ask.Price, flat.Bid.Price, flat.Ask.Price)
		}
		if after.SkewBPS >= 0 {
			t.Fatalf("skew %v bps, want negative for a short", after.SkewBPS)
		}
	})

	t.Run("the inventory limit is USDC of position at the reference", func(t *testing.T) {
		// 10,000 USDC long is 13.74M cNGN: at 13.5M the next $1,000 rung would breach it.
		res, err := BuildQuotes(cfg, spec, perpSnapshot(cash, 13_500_000, livePerp()))
		if err != nil {
			t.Fatalf("BuildQuotes: %v", err)
		}
		if res.Bid != nil || res.BidSuppression == nil || res.BidSuppression.Reason != "max_long_inventory" {
			t.Fatalf("bid %+v suppression %+v, want max_long_inventory", res.Bid, res.BidSuppression)
		}
		if res.Ask == nil {
			t.Fatalf("ask suppressed too: %+v", res.AskSuppression)
		}
	})
}
