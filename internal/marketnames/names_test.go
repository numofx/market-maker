package marketnames

import (
	"reflect"
	"testing"
)

func TestFallbackCoversBothSpellingsOfBothMarkets(t *testing.T) {
	cases := map[string][]string{
		SpotCanonical: {SpotLegacy},
		SpotLegacy:    {SpotCanonical},
		PerpCanonical: {PerpLegacy},
		PerpLegacy:    {PerpCanonical},
		"BTC-PERP":    nil,
	}
	for name, want := range cases {
		if got := FallbackAliases(name); !reflect.DeepEqual(got, want) {
			t.Errorf("FallbackAliases(%q) = %v, want %v", name, got, want)
		}
	}
	for _, name := range []string{SpotCanonical, SpotLegacy} {
		if !IsSpot(name) || IsPerp(name) {
			t.Errorf("%q: IsSpot=%v IsPerp=%v", name, IsSpot(name), IsPerp(name))
		}
	}
	for _, name := range []string{PerpCanonical, PerpLegacy} {
		if IsSpot(name) || !IsPerp(name) {
			t.Errorf("%q: IsSpot=%v IsPerp=%v", name, IsSpot(name), IsPerp(name))
		}
	}
	// Exact match only, like the venue.
	for _, name := range []string{"cngn-usdc", "USDCCNGN-SPOT", "cNGN-SPOT", ""} {
		if IsSpot(name) || IsPerp(name) || FallbackAliases(name) != nil {
			t.Errorf("%q matched inexactly", name)
		}
	}
}
