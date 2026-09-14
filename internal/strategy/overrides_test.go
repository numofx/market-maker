package strategy

import (
	"math"
	"testing"

	"github.com/numofx/market-maker/internal/exchange"
)

func buildWith(t *testing.T, ov Overrides) Result {
	t.Helper()
	res, err := BuildQuotesWithOverrides(baseCfg(), spotSpec(), spotSnapshot(1370, 1374, 1000, 1_000_000), ov)
	if err != nil {
		t.Fatalf("BuildQuotesWithOverrides: %v", err)
	}
	return res
}

func TestNoOverridesIsExactlyBuildQuotes(t *testing.T) {
	want, err := BuildQuotes(baseCfg(), spotSpec(), spotSnapshot(1370, 1374, 1000, 1_000_000))
	if err != nil {
		t.Fatalf("BuildQuotes: %v", err)
	}
	got := buildWith(t, NoOverrides())
	if *got.Bid != *want.Bid || *got.Ask != *want.Ask {
		t.Fatalf("got %+v/%+v want %+v/%+v", *got.Bid, *got.Ask, *want.Bid, *want.Ask)
	}
}

// A positive shift is "lean up": both quotes move up by the same fraction of the mid, and the
// reported reference -- which risk and the terminal read as the market -- does not move at all.
func TestMidShiftRaisesBothQuotesInBpsOfTheMid(t *testing.T) {
	base := buildWith(t, NoOverrides())
	ov := NoOverrides()
	ov.MidShiftBPS = 100
	shifted := buildWith(t, ov)

	for name, pair := range map[string][2]float64{
		"bid": {base.Bid.Price, shifted.Bid.Price},
		"ask": {base.Ask.Price, shifted.Ask.Price},
	} {
		ratio := pair[1] / pair[0]
		if math.Abs(ratio-1.01) > 1e-9 {
			t.Fatalf("%s moved by x%.12f, want x1.01", name, ratio)
		}
	}
	if shifted.ReferencePrice != base.ReferencePrice {
		t.Fatalf("reference moved from %v to %v; only the pricing mid should", base.ReferencePrice, shifted.ReferencePrice)
	}
}

func TestSpreadAddWidensEachSide(t *testing.T) {
	base := buildWith(t, NoOverrides())
	ov := NoOverrides()
	ov.SpreadAddBPS = 20
	wide := buildWith(t, ov)
	if !(wide.Bid.Price < base.Bid.Price && wide.Ask.Price > base.Ask.Price) {
		t.Fatalf("spread add did not widen: base %v/%v wide %v/%v", base.Bid.Price, base.Ask.Price, wide.Bid.Price, wide.Ask.Price)
	}
	// 10 bps configured + 20 added = 30 bps each side of a 1372 mid.
	if want := 1372 * (1 - 0.003); math.Abs(wide.Bid.Price-want) > 1e-9 {
		t.Fatalf("bid %v, want %v", wide.Bid.Price, want)
	}
}

func TestSizeMultScalesAndZeroQuotesNothing(t *testing.T) {
	ov := NoOverrides()
	ov.SizeMult = 0.5
	half := buildWith(t, ov)
	if half.Bid.Size != 2.5 || half.Ask.Size != 2.5 {
		t.Fatalf("sizes %v/%v, want 2.5/2.5", half.Bid.Size, half.Ask.Size)
	}

	ov.SizeMult = 0
	none := buildWith(t, ov)
	if none.Bid != nil || none.Ask != nil || len(none.Bids) != 0 || len(none.Asks) != 0 {
		t.Fatalf("size_mult 0 still quoted: %+v", none)
	}
}

// A pulled side goes through the bid-only/ask-only suppression, with its own reason so a log line
// says it was the operator's terminal and not MM_OPERATOR_MODE.
func TestDisabledSideIsSuppressedAndTheOtherKeepsQuoting(t *testing.T) {
	ov := NoOverrides()
	ov.BidDisabled = true
	res := buildWith(t, ov)
	if res.Bid != nil || len(res.Bids) != 0 {
		t.Fatalf("bid still quoted: %+v", res.Bids)
	}
	if res.BidSuppression == nil || res.BidSuppression.Reason != "control_side_disabled" || res.BidSuppression.Side != exchange.SideBuy {
		t.Fatalf("bid suppression = %+v", res.BidSuppression)
	}
	if res.Ask == nil || len(res.Asks) != 1 {
		t.Fatalf("ask should keep quoting, got %+v", res.Asks)
	}
}
