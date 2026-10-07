package marketdata

import "testing"

// Prices are USDC per cNGN; the index is the local venue's 0.000728.
func TestPerpReference(t *testing.T) {
	const index = 0.000728
	cases := []struct {
		name       string
		bid, ask   float64
		want       float64
		wantSource string
	}{
		{"no book: the index", 0, 0, index, "index"},
		{"one-sided: the index", 0.000727, 0, index, "index"},
		{"crossed: the index", 0.000730, 0.000727, index, "index"},
		{"inside the band: the mid", 0.000727, 0.000731, 0.000729, "book"},
		{"above the band: clamped", 0.000740, 0.000742, index * 1.01, "book_clamped"},
		{"below the band: clamped", 0.000715, 0.000717, index * 0.99, "book_clamped"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, source := PerpReference(tc.bid, tc.ask, index, 100)
			if diff := got - tc.want; diff > 1e-15 || diff < -1e-15 || source != tc.wantSource {
				t.Fatalf("PerpReference = %v/%s, want %v/%s", got, source, tc.want, tc.wantSource)
			}
		})
	}
}
