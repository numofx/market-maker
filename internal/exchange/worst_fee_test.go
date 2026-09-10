package exchange

import (
	"math/big"
	"testing"
)

// spotSpec is the spot market as /v1/markets reports it once fees are live: a 25 bps taker
// schedule, published by markets-service rather than configured here.
func spotSpec() MarketSpec {
	return MarketSpec{Symbol: "USDCcNGN-SPOT", TakerFeeBps: 25}
}

func mustWorstFee(t *testing.T, spec MarketSpec, cfg ClientConfig, enginePrice float64) *big.Int {
	t.Helper()
	c := &HTTPClient{cfg: cfg}
	got, err := c.signedWorstFee(spec, enginePrice)
	if err != nil {
		t.Fatalf("signedWorstFee(%v): %v", enginePrice, err)
	}
	out, ok := new(big.Int).SetString(got, 10)
	if !ok {
		t.Fatalf("signedWorstFee returned %q, which is not an integer", got)
	}
	return out
}

// The bound the contract checks is a quote amount PER BASE UNIT, so it scales with the price.
// This is the whole reason a configured constant cannot be right: the same order at half the
// price needs half the bound.
func TestSignedWorstFeeScalesWithEnginePrice(t *testing.T) {
	// 1/1376 USDC per cNGN, the engine price of a 1376 cNGN/USDC quote.
	at1376 := mustWorstFee(t, spotSpec(), ClientConfig{}, 1.0/1376.0)
	at688 := mustWorstFee(t, spotSpec(), ClientConfig{}, 1.0/688.0)

	if at1376.Sign() <= 0 {
		t.Fatalf("bound at the live price is %s, which bounds nothing", at1376)
	}
	// Twice the engine price, twice the per-unit fee, so twice the bound (within the rounding-up
	// of a single wei on each side).
	doubled := new(big.Int).Mul(at1376, big.NewInt(2))
	if diff := new(big.Int).Sub(at688, doubled); diff.CmpAbs(big.NewInt(2)) > 0 {
		t.Fatalf("bound at 2x price is %s, want ~%s", at688, doubled)
	}
}

// The bound must admit the fee the matcher actually computes, which is
// takerFeeBps/10_000 * price * amount / 1e18, truncated. The contract then compares
// fee/amountFilled against the bound. This reproduces both sides in integers and asserts the
// comparison the chain will make.
func TestSignedWorstFeeCoversTheChargeTheMatcherComputes(t *testing.T) {
	const enginePrice = 1.0 / 1376.0
	scale := new(big.Int).Set(decimalScale)
	bound := mustWorstFee(t, spotSpec(), ClientConfig{}, enginePrice)
	priceWei, ok := new(big.Int).SetString(floatToRaw(enginePrice), 10)
	if !ok {
		t.Fatal("engine price did not encode")
	}

	// A range of fill sizes, including one cNGN, where the truncation is harshest.
	for _, cngn := range []int64{1, 7, 1_000, 5_480, 999_983} {
		amount := new(big.Int).Mul(big.NewInt(cngn), scale)

		fee := new(big.Int).Mul(priceWei, amount)
		fee.Div(fee, scale)
		fee.Mul(fee, big.NewInt(int64(spotSpec().TakerFeeBps)))
		fee.Div(fee, big.NewInt(10_000))

		// TradeModule reverts when fee/amountFilled > worstFee, i.e. when
		// fee * 1e18 > worstFee * amountFilled.
		lhs := new(big.Int).Mul(fee, scale)
		rhs := new(big.Int).Mul(bound, amount)
		if lhs.Cmp(rhs) > 0 {
			t.Fatalf("fill of %d cNGN: fee %s breaches signed bound %s", cngn, fee, bound)
		}
	}
}

// The headroom is what keeps orders already resting on the book alive across a fee change. Without
// it a schedule that ticks up by one basis point reverts every quote signed a second earlier.
func TestSignedWorstFeeLeavesHeadroomAboveTheSchedule(t *testing.T) {
	const enginePrice = 1.0 / 1376.0
	spec := spotSpec()
	bound := mustWorstFee(t, spec, ClientConfig{}, enginePrice)

	// The bound the schedule alone would justify: feeBps/10_000 of the per-unit notional.
	atSchedule := new(big.Rat).Mul(
		ratFromFloat(enginePrice),
		big.NewRat(int64(spec.TakerFeeBps), 10_000),
	)
	atSchedule.Mul(atSchedule, new(big.Rat).SetInt(decimalScale))
	exact := new(big.Int).Quo(atSchedule.Num(), atSchedule.Denom())

	if bound.Cmp(exact) <= 0 {
		t.Fatalf("signed bound %s carries no headroom over the %d bps schedule (%s)", bound, spec.TakerFeeBps, exact)
	}

	// And the headroom is the stated amount, not an arbitrary cushion: 30 bps over a 25 bps
	// schedule, the same ratio the trading app signs.
	want := new(big.Int).Mul(exact, big.NewInt(int64(spec.TakerFeeBps+worstFeeHeadroomBps)))
	want.Div(want, big.NewInt(int64(spec.TakerFeeBps)))
	if diff := new(big.Int).Sub(bound, want); diff.CmpAbs(big.NewInt(2)) > 0 {
		t.Fatalf("signed bound %s, want ~%s", bound, want)
	}
}

// A market that publishes no schedule is the futures path, where the notional is the contract's
// rather than the price's. Nothing here may start signing a different bound for it.
func TestSignedWorstFeeFallsBackToConfigWhenNoScheduleIsPublished(t *testing.T) {
	c := &HTTPClient{cfg: ClientConfig{WorstFee: "0"}}
	got, err := c.signedWorstFee(MarketSpec{Symbol: "USDCcNGN-SEP16-2026"}, 1376)
	if err != nil {
		t.Fatalf("signedWorstFee: %v", err)
	}
	if got != "0" {
		t.Fatalf("worst fee = %q, want the configured %q", got, "0")
	}
}

// A price of zero cannot bound anything, and signing a zero bound against a live schedule is a
// guaranteed revert. Refuse the order instead of resting one that cannot fill.
func TestSignedWorstFeeRefusesAnUnusablePrice(t *testing.T) {
	c := &HTTPClient{cfg: ClientConfig{WorstFee: "0"}}
	if _, err := c.signedWorstFee(spotSpec(), 0); err == nil {
		t.Fatal("a zero engine price must not produce a signed bound")
	}
}
