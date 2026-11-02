package pricing

import (
	"math"
	"testing"
)

func near(t *testing.T, name string, got, want, rel float64) {
	t.Helper()
	if math.Abs(got-want) > rel*math.Abs(want) {
		t.Fatalf("%s = %.12g, want %.12g (rel %.2g)", name, got, want, rel)
	}
}

// The engine's own mark, read on Base mainnet 2026-11-02 at block ~53396700: getIsolatedMargin(1, 0.00072128, 1796047200,
// false, -1e24, true) marked 1M cNGN of the put at -32.158540772484 USDC with the feed's 40% ATM surface. Seconds to
// expiry at that block were ~2,460,600 (expiry 1796047200 less the block's timestamp); forward 0.000721285311861612.
// Black-76 on the same inputs lands on the same premium to well under a basis point.
func TestPutMatchesTheEngineMark(t *testing.T) {
	put := Option{Forward: 0.000721285311861612, Strike: 0.00072128, Vol: 0.400001472998962986, Seconds: 2_460_600, Call: false}
	near(t, "1M cNGN put", put.Price()*1_000_000, 32.158540772484, 2e-3)
}

func TestPutCallParityWithoutDiscounting(t *testing.T) {
	for _, k := range []float64{0.00062, 0.00072, 0.00082} {
		call := Option{Forward: 0.000721, Strike: k, Vol: 0.4, Seconds: 25 * 86400, Call: true}
		put := call
		put.Call = false
		near(t, "C - P = F - K", call.Price()-put.Price(), 0.000721-k, 1e-9)
		near(t, "delta(C) - delta(P) = 1", call.Delta()-put.Delta(), 1, 1e-12)
	}
}

func TestATMApproximation(t *testing.T) {
	o := Option{Forward: 0.000721, Strike: 0.000721, Vol: 0.4, Seconds: 29 * 86400, Call: true}
	approx := o.Forward * o.Vol * math.Sqrt(o.Years()) / math.Sqrt(2*math.Pi)
	near(t, "ATM ~ F sigma sqrt(t) / sqrt(2 pi)", o.Price(), approx, 2e-3)
	near(t, "ATM vega = price / vol, to first order", o.Vega(), o.Price()/o.Vol, 1e-2)
}

func TestExpiredIsIntrinsic(t *testing.T) {
	call := Option{Forward: 0.0008, Strike: 0.00072, Vol: 0.4, Seconds: 0, Call: true}
	near(t, "expired ITM call", call.Price(), 0.00008, 1e-12)
	if (Option{Forward: 0.0007, Strike: 0.00072, Vol: 0.4, Call: true}).Price() != 0 {
		t.Fatal("an expired OTM call is worth nothing")
	}
	if call.Vega() != 0 || call.Delta() != 1 {
		t.Fatalf("expired ITM call: vega %v delta %v", call.Vega(), call.Delta())
	}
}

func TestVolMovesThePremiumByVega(t *testing.T) {
	o := Option{Forward: 0.000721, Strike: 0.00067, Vol: 0.40, Seconds: 20 * 86400}
	up := o.AtVol(0.43)
	near(t, "3 vol points ~ 3 x vega/100", up.Price()-o.Price(), 0.03*o.Vega(), 5e-2)
	if !(up.Price() > o.Price()) {
		t.Fatal("more vol, more premium")
	}
}
