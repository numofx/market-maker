package execution

import (
	"testing"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
)

// A production rung: 40 USDC at ~0.000728 USDC per cNGN is 54,945 whole cNGN.
const (
	rungPrice = 0.000728
	rungSize  = 54945.0
)

var spotSpec = exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", MinSize: 1}

// The venue cannot express a fraction of a cNGN, so one cNGN of difference is rounding, not a
// size change, and must not replace the order. On a small rung (1,000 cNGN, where 5 bps is half
// a cNGN) only the quantum floor keeps it.
func TestAOneCNGNDifferenceIsNotAMismatch(t *testing.T) {
	cfg := config.Config{AdoptSizeTolerance: 0.000001}
	quantum := sizeQuantum(spotSpec)
	if quantum != 1 {
		t.Fatalf("spot quantum = %v, want one whole cNGN", quantum)
	}
	const smallRung = 1000.0
	if !sizeMismatchRequiresReplace(smallRung-1, smallRung, cfg, 0) {
		t.Fatal("without the quantum floor a one-cNGN gap must replace -- otherwise the test proves nothing")
	}
	if sizeMismatchRequiresReplace(smallRung-1, smallRung, cfg, quantum) {
		t.Fatal("a one-cNGN gap is the venue's own rounding and must be kept")
	}
}

// The target is MM_ORDER_SIZE in USDC at the reference, so a price move of x bps moves it by ~x
// bps. Below MM_CANCEL_STALE_ORDER_THRESHOLD_BPS (5 by default) the price is not a replace, and the
// matching size drift must not be one either: the relative dust tolerance is the same 5 bps.
func TestASubThresholdPriceMoveDoesNotChurnOnSize(t *testing.T) {
	cfg := config.Config{AdoptSizeTolerance: 0.000001}
	for _, bps := range []float64{0.5, 1, 2, 4, 4.9} {
		target := float64(int(40 / (rungPrice * (1 + bps/10000))))
		if sizeMismatchRequiresReplace(rungSize, target, cfg, sizeQuantum(spotSpec)) {
			t.Errorf("a %.1f bps move retargets %v -> %v cNGN and would churn", bps, rungSize, target)
		}
	}
}

// The floor must not swallow differences that matter. A real size change -- an operator doubling
// MM_ORDER_SIZE, a size_mult, a price move over the replace threshold -- still replaces.
func TestARealSizeChangeStillReplaces(t *testing.T) {
	cfg := config.Config{AdoptSizeTolerance: 0.000001}
	quantum := sizeQuantum(spotSpec)
	for _, target := range []float64{rungSize * 2, rungSize * 1.01, rungSize - 100, 2 * rungSize} {
		if !sizeMismatchRequiresReplace(rungSize, target, cfg, quantum) {
			t.Errorf("resting %v against target %v must replace", rungSize, target)
		}
	}
}

func TestSizeQuantumIsOneAtomicUnitOfTheMarket(t *testing.T) {
	// The cNGN markets: one whole cNGN.
	if got := sizeQuantum(spotSpec); got != 1 {
		t.Fatalf("spot quantum = %v, want 1", got)
	}
	if got := sizeQuantum(exchange.MarketSpec{Symbol: "USDCcNGN-PERP", Kind: exchange.MarketKindPerp, MinSize: 1}); got != 1 {
		t.Fatalf("perp quantum = %v, want 1", got)
	}
	// Futures quantise in contract lots; the size is already in contracts.
	future := exchange.MarketSpec{Symbol: "USDCcNGN-SEP16-2026", MinSize: 0.001}
	if got := sizeQuantum(future); got != 0.001 {
		t.Fatalf("future quantum = %v, want the 0.001 contract step", got)
	}
}
