package execution

import (
	"math"
	"testing"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
)

// The live sell level that churned 4,065 times on 2026-09-10, at an unchanged price.
const (
	churnPrice = 1331.9886755300024
	churnSize  = 0.359975
)

var spotSpec = exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", MinSize: 0.000001}

// rests returns what the venue actually holds after flooring a UI size to whole cNGN -- the step
// the spot market quantises on.
func rests(uiSize, price float64) float64 {
	return math.Floor(uiSize*price) / price
}

// The exact production loop: the bot asks for a size the venue cannot express, the venue floors
// it, and the bot then reads the floored order back as a mismatch and replaces it -- with an
// order that will be floored in precisely the same way. Nothing converges; it just burns cancels.
func TestAFlooredOrderIsNotAMismatch(t *testing.T) {
	cfg := config.Config{AdoptSizeTolerance: 0.000001}
	resting := rests(churnSize, churnPrice)

	if resting == churnSize {
		t.Fatal("the premise is wrong: this size is exactly representable, pick another")
	}
	quantum := sizeQuantumUI(spotSpec, churnPrice)

	if !sizeMismatchRequiresReplace(resting, churnSize, cfg, 0) {
		t.Fatal("without the quantum floor this case must replace -- otherwise the test proves nothing")
	}
	if sizeMismatchRequiresReplace(resting, churnSize, cfg, quantum) {
		t.Fatalf(
			"target %.6f rests %.6f (diff %.6f) is pure flooring and must be kept; quantum is %.8f",
			churnSize, resting, math.Abs(churnSize-resting), quantum,
		)
	}
}

// The dust tolerance is relative (5 bps of size) while the flooring error is absolute (up to one
// cNGN), so they cross at ~1.5 USDC and every smaller order churned. Sweeping the ladder proves
// the floor covers the whole range rather than just the one size that happened to be logged.
func TestNoLadderSizeChurnsOnFlooringAlone(t *testing.T) {
	cfg := config.Config{AdoptSizeTolerance: 0.000001}
	for _, size := range []float64{0.05, 0.359975, 0.72, 1.2, 1.44, 1.728, 5.0} {
		resting := rests(size, churnPrice)
		if sizeMismatchRequiresReplace(resting, size, cfg, sizeQuantumUI(spotSpec, churnPrice)) {
			t.Errorf("size %.6f rests %.6f and still replaces -- it would churn every cycle", size, resting)
		}
	}
}

// The floor must not swallow differences that matter. A whole cNGN more than the quantum is a real
// size change and still has to be replaced, or the ladder silently stops tracking the target.
func TestARealSizeChangeStillReplaces(t *testing.T) {
	cfg := config.Config{AdoptSizeTolerance: 0.000001}
	quantum := sizeQuantumUI(spotSpec, churnPrice)

	for _, mult := range []float64{2, 5, 100} {
		resting := churnSize - mult*quantum
		if !sizeMismatchRequiresReplace(resting, churnSize, cfg, quantum) {
			t.Errorf("a %.0f-quantum difference must still replace (target %.6f, rests %.6f)", mult, churnSize, resting)
		}
	}
}

func TestSizeQuantumIsOneAtomicUnitOfTheMarket(t *testing.T) {
	// Spot: one whole cNGN, expressed in the USDC the quote is denominated in.
	if got, want := sizeQuantumUI(spotSpec, churnPrice), 1.0/churnPrice; math.Abs(got-want) > 1e-15 {
		t.Fatalf("spot quantum = %v, want %v", got, want)
	}
	// A nonsense price cannot produce a quantum, and must not produce a huge one that swallows
	// every real size change.
	if got := sizeQuantumUI(spotSpec, 0); got != 0 {
		t.Fatalf("quantum at price 0 = %v, want 0", got)
	}
	// Futures quantise in contract lots; the size is already in contracts.
	future := exchange.MarketSpec{Symbol: "USDCcNGN-SEP16-2026", MinSize: 0.001}
	if got := sizeQuantumUI(future, churnPrice); got != 0.001 {
		t.Fatalf("future quantum = %v, want the 0.001 contract step", got)
	}
}
