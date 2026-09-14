package execution

import (
	"testing"
	"time"

	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/strategy"
)

// The bot never cancels a protected order: s.cancel skips it and reports success, so reconcileSide
// frees the slot and places a duplicate beside it. Neither expiry rule may therefore fire for one, or
// a single validation: order signed for 3600s (cmd/acceptance-cross still does) produces a place and a
// no_target cancel on every poll for an hour.
func TestExpiryRulesLeaveProtectedOrdersAlone(t *testing.T) {
	now := time.Now().UTC()
	cfg := expiryCfg()
	cfg.ProtectedOrderIDPrefixes = []string{"validation:", "test:"}
	target := &strategy.Quote{Side: exchange.SideBuy, Price: 1326, Size: 1.2}

	t.Run("signed longer than configured", func(t *testing.T) {
		order := restingAtTarget("validation:cross:1", now.Unix()+3400)
		if got := staleTermsReason(cfg, &order, "1000"); got != "" {
			t.Fatalf("staleTermsReason = %q for a protected order, want none", got)
		}
		if d := evaluateCancel(&order, target, nil, cfg, time.Time{}, now, 0, "1000"); d.Cancel {
			t.Fatalf("evaluateCancel cancelled a protected order (reason %q)", d.Reason)
		}
	})

	t.Run("inside the expiry margin", func(t *testing.T) {
		order := restingAtTarget("test:probe", now.Unix()+5)
		if d := evaluateCancel(&order, target, nil, cfg, time.Time{}, now, 0, "1000"); d.Cancel {
			t.Fatalf("evaluateCancel rolled a protected order (reason %q)", d.Reason)
		}
	})

	t.Run("a managed order in the same position is still rolled", func(t *testing.T) {
		order := restingAtTarget("mm:USDCcNGN-SPOT:buy:1", now.Unix()+5)
		if d := evaluateCancel(&order, target, nil, cfg, time.Time{}, now, 0, "1000"); !d.Cancel || d.Reason != cancelReasonExpiring {
			t.Fatalf("decision %+v, want the managed quote rolled", d)
		}
	})
}
