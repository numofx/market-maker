package strategy

import (
	"math"
	"testing"

	"github.com/numofx/market-maker/internal/exchange"
)

// Fills each side of the perp ladder through the same translation the bot signs with, applies the
// fill to the on-chain NGN position, reads the position back the way GetBalances does, requotes,
// and checks direction, sign and lean in both units:
//
//   - displayed: NGN per USD, sized in USD, a UI long is long USD;
//   - on chain: USD per NGN, sized in NGN, long NGN positive -- every side and sign flipped.
//
// A bot that got any one of the flips wrong would lean INTO its inventory: after being lifted it
// would quote cheaper and keep selling.

const fillIndex = 1374.0

func TestPerpFillEachSide(t *testing.T) {
	cfg := baseCfg()
	cfg.PerpMaxLeverage = 1.5
	cfg.OrderSize = 1_000
	cfg.InventorySkewBPS = 50
	cfg.MaxLongInventory, cfg.MaxShortInventory = 10_000, -10_000

	spec := perpSpec()
	spec.AssetAddress, spec.SubID, spec.QuoteAddress = "0xperp", "0", "0xcash"
	spec.Perp = &exchange.PerpState{IndexPriceUI: fillIndex}
	const cash = 20_000.0

	quote := func(t *testing.T, uiPosition float64) Result {
		t.Helper()
		perp := livePerp()
		res, err := BuildQuotes(cfg, spec, perpSnapshot(cash, uiPosition, perp))
		if err != nil || res.Bid == nil || res.Ask == nil {
			t.Fatalf("BuildQuotes at position %v: %v (bid %v ask %v)", uiPosition, err, res.Bid, res.Ask)
		}
		return res
	}
	// The engine price of a displayed quote, USD per NGN.
	engine := func(t *testing.T, q *Quote) (exchange.Side, float64, float64) {
		t.Helper()
		side, price, amount, err := exchange.EngineOrderFromUI(spec, q.Side, q.Price, q.Size)
		if err != nil {
			t.Fatalf("EngineOrderFromUI: %v", err)
		}
		return side, price, amount
	}
	// What GetBalances reports after the fill moves the NGN position by engineDelta.
	positionAfter := func(t *testing.T, engineDelta float64) float64 {
		t.Helper()
		balances, err := exchange.PerpBalances(spec, map[string]float64{"0xperp|0": engineDelta, "0xcash|0": cash})
		if err != nil {
			t.Fatalf("PerpBalances: %v", err)
		}
		return balances[0].Total
	}

	flat := quote(t, 0)
	_, flatBidEngine, _ := engine(t, flat.Bid)
	_, flatAskEngine, _ := engine(t, flat.Ask)

	t.Run("bid filled: the bot is long USD, short the NGN perp, and leans to sell", func(t *testing.T) {
		side, price, amount := engine(t, flat.Bid)
		// Displayed: a buy of USD. On chain: a SELL of NGN at 1/price.
		if side != exchange.SideSell {
			t.Fatalf("a UI bid must reach the engine as a sell of NGN, got %s", side)
		}
		if math.Abs(price-1/flat.Bid.Price) > 1e-15 || math.Abs(amount-flat.Bid.Size*flat.Bid.Price) > 1e-6 {
			t.Fatalf("engine price/amount %v/%v for UI %v x %v", price, amount, flat.Bid.Price, flat.Bid.Size)
		}
		// Selling NGN leaves the engine position negative; the bot reads it as a positive (long) USD position.
		enginePosition := -amount
		ui := positionAfter(t, enginePosition)
		if !(enginePosition < 0 && ui > 0) {
			t.Fatalf("engine %v NGN / UI %v USD: want engine short, UI long", enginePosition, ui)
		}
		if want := amount / fillIndex; math.Abs(ui-want) > 1e-6 {
			t.Fatalf("UI position %v, want %v (engine NGN at the index)", ui, want)
		}

		after := quote(t, ui)
		// Displayed: long USD, so both quotes move DOWN in NGN per USD -- buy less, sell more.
		if !(after.Bid.Price < flat.Bid.Price && after.Ask.Price < flat.Ask.Price) {
			t.Fatalf("displayed quotes %v/%v did not lean down from %v/%v", after.Bid.Price, after.Ask.Price, flat.Bid.Price, flat.Ask.Price)
		}
		// On chain: short NGN, so both move UP in USD per NGN -- the engine bid for NGN (the UI ask)
		// pays more to buy NGN back, and the engine ask (the UI bid) asks more to sell further.
		askSide, askEngine, _ := engine(t, after.Ask)
		_, bidEngine, _ := engine(t, after.Bid)
		if askSide != exchange.SideBuy {
			t.Fatalf("a UI ask must reach the engine as a buy of NGN, got %s", askSide)
		}
		if !(askEngine > flatAskEngine && bidEngine > flatBidEngine) {
			t.Fatalf("engine prices %v/%v did not lean up from %v/%v", askEngine, bidEngine, flatAskEngine, flatBidEngine)
		}
	})

	t.Run("ask filled: the bot is short USD, long the NGN perp, and leans to buy", func(t *testing.T) {
		side, price, amount := engine(t, flat.Ask)
		if side != exchange.SideBuy {
			t.Fatalf("a UI ask must reach the engine as a buy of NGN, got %s", side)
		}
		if math.Abs(price-1/flat.Ask.Price) > 1e-15 {
			t.Fatalf("engine price %v for UI %v", price, flat.Ask.Price)
		}
		enginePosition := amount
		ui := positionAfter(t, enginePosition)
		if !(enginePosition > 0 && ui < 0) {
			t.Fatalf("engine %v NGN / UI %v USD: want engine long, UI short", enginePosition, ui)
		}

		after := quote(t, ui)
		if !(after.Bid.Price > flat.Bid.Price && after.Ask.Price > flat.Ask.Price) {
			t.Fatalf("displayed quotes %v/%v did not lean up from %v/%v", after.Bid.Price, after.Ask.Price, flat.Bid.Price, flat.Ask.Price)
		}
		_, askEngine, _ := engine(t, after.Ask)
		_, bidEngine, _ := engine(t, after.Bid)
		if !(askEngine < flatAskEngine && bidEngine < flatBidEngine) {
			t.Fatalf("engine prices %v/%v did not lean down from %v/%v", askEngine, bidEngine, flatAskEngine, flatBidEngine)
		}
	})
}
