package marketdata

import "testing"

func TestPerpReference(t *testing.T) {
	cases := []struct {
		name       string
		bid, ask   float64
		want       float64
		wantSource string
	}{
		{"no book: the index", 0, 0, 1374, "index"},
		{"one-sided: the index", 1370, 0, 1374, "index"},
		{"crossed: the index", 1380, 1370, 1374, "index"},
		{"inside the band: the mid", 1372, 1378, 1375, "book"},
		{"above the band: clamped", 1400, 1410, 1374 * 1.01, "book_clamped"},
		{"below the band: clamped", 1300, 1310, 1374 * 0.99, "book_clamped"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, source := PerpReference(tc.bid, tc.ask, 1374, 100)
			if diff := got - tc.want; diff > 1e-9 || diff < -1e-9 || source != tc.wantSource {
				t.Fatalf("PerpReference = %v/%s, want %v/%s", got, source, tc.want, tc.wantSource)
			}
		})
	}
}
