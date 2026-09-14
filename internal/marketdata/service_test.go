package marketdata

import (
	"testing"

	"github.com/numofx/market-maker/internal/exchange"
)

// Live on 2026-09-14: the bot's own bid was the best bid, so pricing off it walked the bid toward the
// trader's ask. Only other participants' orders set the top of book.
func TestOthersBestPricesExcludesTheBotsOwnOrders(t *testing.T) {
	book := exchange.Book{
		Bids: []exchange.BookLevel{
			{Price: 1366.23, OrderID: "mm:USDCcNGN-SPOT:buy:1"},
			{Price: 1333.97, OrderID: "spot-trader-bid"},
			{Price: 1364.10, OrderID: "mm:USDCcNGN-SPOT:buy:2"},
		},
		Asks: []exchange.BookLevel{{Price: 1370, OrderID: "spot-trader-ask"}},
	}
	own := []exchange.Order{{ID: "mm:USDCcNGN-SPOT:buy:1"}, {ID: "mm:USDCcNGN-SPOT:buy:2"}}

	bid, ask := othersBestPrices(book, own)
	if bid != 1333.97 || ask != 1370 {
		t.Fatalf("best = (%v, %v), want the trader's (1333.97, 1370)", bid, ask)
	}

	// With no open orders every level is someone else's, and the best is the max bid / min ask
	// regardless of the order the levels arrive in.
	bid, ask = othersBestPrices(book, nil)
	if bid != 1366.23 || ask != 1370 {
		t.Fatalf("best with no own orders = (%v, %v), want (1366.23, 1370)", bid, ask)
	}

	// A level without an order id cannot be matched to the bot, so it counts as someone else's.
	bid, _ = othersBestPrices(exchange.Book{Bids: []exchange.BookLevel{{Price: 1400}}}, []exchange.Order{{ID: ""}})
	if bid != 1400 {
		t.Fatalf("anonymous level bid = %v, want 1400", bid)
	}
}
