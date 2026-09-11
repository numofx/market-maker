package execution

import (
	"testing"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/strategy"
)

func attrMap(t *testing.T, attrs []any) map[string]any {
	t.Helper()
	if len(attrs)%2 != 0 {
		t.Fatalf("odd attr count %d -- slog would drop the last one", len(attrs))
	}
	out := map[string]any{}
	for i := 0; i < len(attrs); i += 2 {
		k, ok := attrs[i].(string)
		if !ok {
			t.Fatalf("attr key %d is not a string: %v", i, attrs[i])
		}
		out[k] = attrs[i+1]
	}
	return out
}

// The line has to answer, on its own, why this order was replaced. Every field below was one I had
// to infer from elsewhere while chasing the churn, and inferred wrong twice.
func TestTheCancelLineCarriesEveryDecisionInput(t *testing.T) {
	current := &exchange.Order{
		ID: "lvl3", Side: exchange.SideSell, Price: 1331.847318, Size: 0.359651, RawSize: "479",
	}
	target := &strategy.Quote{Side: exchange.SideSell, Price: 1331.847318, Size: 0.359975}
	cfg := config.Config{AdoptSizeTolerance: 0.000001}

	got := attrMap(t, sizeMismatchAttrs(current, target, sizeQuantumUI(spotSpec, current.Price), cfg))

	for _, key := range []string{
		"order_id", "side", "current_size", "target_size", "diff", "quantum",
		"tolerance", "tolerance_source", "current_price", "target_price",
		"raw_engine_amount", "size_from_raw",
	} {
		if _, ok := got[key]; !ok {
			t.Errorf("missing %q -- the line cannot explain its own decision without it", key)
		}
	}
	if got["raw_engine_amount"] != "479" {
		t.Errorf("raw_engine_amount = %v, want the untouched Postgres value", got["raw_engine_amount"])
	}
}

// The whole point of raw_engine_amount is to localise a disagreement between what Postgres returns
// and what the size was converted into. If they agree the fault is in the decision; if they do not
// it is in the conversion, and the line has to make that visible rather than leave it to be
// guessed at from the book.
func TestSizeFromRawExposesAConversionDisagreement(t *testing.T) {
	cfg := config.Config{AdoptSizeTolerance: 0.000001}
	target := &strategy.Quote{Side: exchange.SideSell, Price: 1331.847318, Size: 0.359975}

	agreeing := &exchange.Order{
		ID: "ok", Side: exchange.SideSell, Price: 1331.847318, Size: 479 / 1331.847318, RawSize: "479",
	}
	got := attrMap(t, sizeMismatchAttrs(agreeing, target, 0, cfg))
	if diff := got["size_from_raw"].(float64) - agreeing.Size; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("size_from_raw %v should match current_size %v when the conversion is sound",
			got["size_from_raw"], agreeing.Size)
	}

	// A size that does not follow from the raw amount is precisely the bug being hunted.
	disagreeing := &exchange.Order{
		ID: "bad", Side: exchange.SideSell, Price: 1331.847318, Size: 1.2, RawSize: "479",
	}
	got = attrMap(t, sizeMismatchAttrs(disagreeing, target, 0, cfg))
	if got["size_from_raw"].(float64) == disagreeing.Size {
		t.Error("a conversion disagreement must be visible on the line")
	}
}

// Which bound won decides whether the quantum floor was even in play. Getting this wrong is how a
// fix that never applied looked like a fix that did not work.
func TestToleranceSourceNamesTheWinningBound(t *testing.T) {
	for _, tc := range []struct {
		abs, rel, quantum float64
		want              string
	}{
		{0.000001, 0.00018, 0.00075, "quantum"},
		{0.000001, 0.0006, 0.0, "relative_dust_bps"},
		{0.01, 0.0006, 0.00075, "absolute_adopt_tolerance"},
	} {
		if got := toleranceSource(tc.abs, tc.rel, tc.quantum); got != tc.want {
			t.Errorf("toleranceSource(%v,%v,%v) = %q, want %q", tc.abs, tc.rel, tc.quantum, got, tc.want)
		}
	}
}
