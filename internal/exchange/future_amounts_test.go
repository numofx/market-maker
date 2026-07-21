package exchange

import "testing"

func TestFutureOrderAmounts(t *testing.T) {
	spec := MarketSpec{Symbol: "USDCcNGN-APR30-2026", SizeStep: 0.001, MinSize: 0.001}

	tests := []struct {
		name       string
		size       float64
		wantBody   string
		wantSigned string
		wantRound  float64
	}{
		// markets-service normalizes the BODY (÷ MinSize 0.001) to an atomic count and
		// requires the SIGNED wei amount to be an exact integer multiple of it.
		{name: "aligned two contracts", size: 0.002, wantBody: "0.002", wantSigned: "2000000000000000", wantRound: 0.002},
		{name: "rounds down to min size step", size: 0.014524, wantBody: "0.014", wantSigned: "14000000000000000", wantRound: 0.014},
		{name: "single min size unit", size: 0.001, wantBody: "0.001", wantSigned: "1000000000000000", wantRound: 0.001},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, signed, rounded, err := futureOrderAmounts(spec, tt.size)
			if err != nil {
				t.Fatalf("futureOrderAmounts() error = %v", err)
			}
			if body != tt.wantBody {
				t.Fatalf("body = %q want %q", body, tt.wantBody)
			}
			if signed != tt.wantSigned {
				t.Fatalf("signed = %q want %q", signed, tt.wantSigned)
			}
			if rounded != tt.wantRound {
				t.Fatalf("rounded = %v want %v", rounded, tt.wantRound)
			}
		})
	}
}

func TestFutureOrderAmountsBelowMinSizeErrors(t *testing.T) {
	spec := MarketSpec{Symbol: "USDCcNGN-APR30-2026", SizeStep: 0.001, MinSize: 0.001}
	if _, _, _, err := futureOrderAmounts(spec, 0.0004); err == nil {
		t.Fatal("expected error for size below min size step")
	}
}

// A future's resting order is stored (and presented) by markets-service as an ATOMIC
// COUNT of MinSize steps. The read-back must recover the original decimal size, otherwise
// the reconcile loop mistakes its own resting order for a size_mismatch and churns it.
func TestOrderAmountToFloatFuturesAtomicCount(t *testing.T) {
	spec := MarketSpec{Symbol: "USDCcNGN-SEP16-2026", SizeStep: 0.001, MinSize: 0.001}
	cases := map[string]float64{
		"14": 0.014, // the size the bot places (0.014524 rounded down)
		"2":  0.002, // the verified first-fill size
		"1":  0.001, // one min-size unit
	}
	for raw, want := range cases {
		if got := orderAmountToFloat(spec, raw); got != want {
			t.Fatalf("orderAmountToFloat(%q) = %v want %v", raw, got, want)
		}
	}
	// Round-trip: what futureOrderAmounts sends as the body normalizes to this atomic count.
	if got := orderAmountToFloat(spec, "14"); got != 0.014 {
		t.Fatalf("round-trip read-back = %v want 0.014", got)
	}
}

// Spot orders keep the legacy wei scaling and must be untouched by the futures path.
func TestOrderAmountToFloatSpotUnchanged(t *testing.T) {
	spec := MarketSpec{Symbol: "USDCcNGN-SPOT", SizeStep: 0.000001, MinSize: 0.000001}
	if got, want := orderAmountToFloat(spec, "1000000000000000000"), rawOrderSizeToFloat(spec, "1000000000000000000"); got != want {
		t.Fatalf("spot orderAmountToFloat = %v want %v (legacy)", got, want)
	}
}
