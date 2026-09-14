package execution

import (
	"testing"
	"time"
)

// The image before 60s expiry signed 3600s, and its ladder survives the deploy. Matching price, size
// and the other terms must not be enough to keep such an order: its expiry is the exposure bound the
// new config promises. staleTermsReason serves both startup reconciliation and every sync cycle, so
// this covers adoption and steady state alike.
func TestStaleTermsRejectsAnOrderSignedLongerThanTheConfiguredExpiry(t *testing.T) {
	now := time.Now().UTC()
	cfg := expiryCfg() // OrderExpirySeconds 60

	cases := []struct {
		name   string
		expiry int64
		want   string
	}{
		{"signed for an hour by the previous image", now.Unix() + 3400, "expiry_beyond_config"},
		{"signed for the configured sixty seconds", now.Unix() + 60, ""},
		{"inside the clock-skew slack", now.Unix() + 65, ""},
		{"just past the slack", now.Unix() + 60 + signedExpirySlackSeconds + 5, "expiry_beyond_config"},
		{"unknown expiry is left alone", 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			order := restingAtTarget("mm:USDCcNGN-SPOT:buy:1", tc.expiry)
			if got := staleTermsReason(cfg, &order, ""); got != tc.want {
				t.Fatalf("staleTermsReason = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExpiryBeyondConfigIsOffWithoutAConfiguredExpiry(t *testing.T) {
	cfg := expiryCfg()
	cfg.OrderExpirySeconds = 0
	order := restingAtTarget("o", time.Now().Unix()+86_400)
	if expiryBeyondConfig(cfg, &order, time.Now()) {
		t.Fatal("with no configured expiry there is nothing to compare against; the order must be left alone")
	}
}
