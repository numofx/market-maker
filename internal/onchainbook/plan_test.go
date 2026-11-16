package onchainbook

import (
	"math/big"
	"testing"
	"time"
)

func e18(f float64) *big.Int {
	v, _ := new(big.Float).Mul(big.NewFloat(f), big.NewFloat(1e18)).Int(nil)
	return v
}

var (
	tick = big.NewInt(1e12)
	now  = time.Unix(1_794_840_000, 0)
)

func params() Params {
	return Params{
		HalfSpreadTicks: 100, BidCurve: 1, AskCurve: 1, MaxSkewTicks: 200, TTL: time.Hour,
		RepriceTicks: 20, RefreshAfter: 30 * time.Minute, CutoffBeforeLastTrade: time.Hour, MaxDivergenceBPS: 50,
	}
}

func inputs() Inputs {
	// the live reads of 2026-11-16 14:58 UTC: forward 1.101093713, index 1.100598531, push 1.10021388
	return Inputs{
		Now:           now,
		Index:         big.NewInt(0).SetUint64(1_100_598_531_463_527_623),
		Forward:       big.NewInt(0).SetUint64(1_101_093_713_422_746_282),
		Reference:     big.NewInt(0).SetUint64(1_100_213_880_000_000_000),
		TickSize:      tick,
		LastTradeTime: time.Unix(1_796_049_000, 0),
	}
}

// live is the spline Decide would have sent at `at`, still standing.
func live(offset int32, at time.Time) Spline {
	p := params()
	return Spline{OffsetTicks: offset, HalfSpreadTicks: p.HalfSpreadTicks, BidCurve: 1, AskCurve: 1,
		MaxSkewTicks: p.MaxSkewTicks, TTL: 3600, UpdatedAt: at, Seq: uint64(at.UnixMilli()), Active: true, Owner: maker}
}

func TestTheLiveReadsGiveTheForwardsBasisInTicks(t *testing.T) {
	d := Decide(params(), inputs(), Spline{})
	if d.Action != ActionUpdate || d.Reason != "down" {
		t.Fatalf("a spline that is down is sent: %+v", d)
	}
	// (1.101093713422746282 - 1.100598531463527623) / 1e-6 = 495.18 -> 495
	if d.Target.OffsetTicks != 495 || d.Target.HalfSpreadTicks != 100 || d.Target.TTL != 3600 || !d.Target.Active {
		t.Fatalf("target %+v", d.Target)
	}
	if d.Target.Seq != uint64(now.UnixMilli()) {
		t.Fatalf("seq %d is not the clock", d.Target.Seq)
	}
}

func TestOffsetRoundsHalfAwayFromZero(t *testing.T) {
	cases := []struct {
		diff int64
		want int32
	}{{0, 0}, {499_999_999_999, 0}, {500_000_000_000, 1}, {-500_000_000_000, -1}, {-1_499_999_999_999, -1},
		{-1_500_000_000_000, -2}, {2_000_000_000_000, 2}}
	base := e18(1.1)
	for _, c := range cases {
		got, ok := OffsetTicks(new(big.Int).Add(base, big.NewInt(c.diff)), base, tick)
		if !ok || got != c.want {
			t.Errorf("diff %d: got %d ok=%v, want %d", c.diff, got, ok, c.want)
		}
	}
	if _, ok := OffsetTicks(e18(3000), e18(1), tick); ok {
		t.Error("an offset past int32 is refused")
	}
	if _, ok := OffsetTicks(e18(1), e18(1), big.NewInt(0)); ok {
		t.Error("a zero tick is refused")
	}
}

func TestEveryWayItCannotPriceTakesTheSplineDown(t *testing.T) {
	broken := map[string]func(*Inputs){
		"cutoff_before_last_trade":   func(in *Inputs) { in.LastTradeTime = now.Add(time.Hour) },
		"index_unreadable":           func(in *Inputs) { in.Index = nil },
		"forward_unreadable":         func(in *Inputs) { in.Forward = nil },
		"reference_unreadable":       func(in *Inputs) { in.Reference = nil },
		"index_reference_divergence": func(in *Inputs) { in.Index, in.Reference = e18(1.1), e18(1.1*1.0051) },
		"offset_out_of_range":        func(in *Inputs) { in.Forward = e18(5000) },
	}
	for reason, mutate := range broken {
		in := inputs()
		mutate(&in)
		if reason == "offset_out_of_range" {
			in.Reference, in.Index = e18(1.1), e18(1.1)
		}
		d := Decide(params(), in, live(495, now.Add(-time.Minute)))
		if d.Action != ActionCancel || d.Reason != reason {
			t.Errorf("%s with a live spline: %+v", reason, d)
		}
		d = Decide(params(), in, Spline{Seq: 7})
		if d.Action != ActionNone || d.Reason != reason {
			t.Errorf("%s with the spline already down: %+v (no transaction to send)", reason, d)
		}
	}
}

func TestTheCutoffIsInclusiveAndTheDivergenceBoundIsNot(t *testing.T) {
	in := inputs()
	in.LastTradeTime = now.Add(time.Hour) // now == last trade - cutoff
	if d := Decide(params(), in, live(495, now)); d.Action != ActionCancel {
		t.Fatalf("at the cutoff exactly: %+v", d)
	}
	in = inputs()
	in.Index, in.Reference = e18(1.1), e18(1.1*1.0049)
	if d := Decide(params(), in, Spline{}); d.Action != ActionUpdate {
		t.Fatalf("49 bps apart quotes: %+v", d)
	}
}

func TestUpdatesOnlyWhenWorthATransaction(t *testing.T) {
	p := params()
	recent := now.Add(-time.Minute)
	cases := []struct {
		name   string
		cur    Spline
		action Action
		reason string
	}{
		{"unchanged", live(495, recent), ActionNone, "unchanged"},
		{"a move under the threshold", live(495-19, recent), ActionNone, "unchanged"},
		{"a move at the threshold", live(495-20, recent), ActionUpdate, "repriced"},
		{"a move down at the threshold", live(495+20, recent), ActionUpdate, "repriced"},
		{"due a refresh", live(495, now.Add(-p.RefreshAfter)), ActionUpdate, "refresh"},
		{"lapsed", live(495, now.Add(-p.TTL-time.Second)), ActionUpdate, "down"},
		{"cancelled", func() Spline { s := live(495, recent); s.Active = false; return s }(), ActionUpdate, "down"},
		{"half-spread changed", func() Spline { s := live(495, recent); s.HalfSpreadTicks = 90; return s }(), ActionUpdate, "config_changed"},
		{"curve changed", func() Spline { s := live(495, recent); s.AskCurve = 2; return s }(), ActionUpdate, "config_changed"},
		{"ttl changed", func() Spline { s := live(495, recent); s.TTL = 600; return s }(), ActionUpdate, "config_changed"},
		{"skew changed", func() Spline { s := live(495, recent); s.MaxSkewTicks = 0; return s }(), ActionUpdate, "config_changed"},
	}
	for _, c := range cases {
		d := Decide(p, inputs(), c.cur)
		if d.Action != c.action || d.Reason != c.reason {
			t.Errorf("%s: got %s/%s, want %s/%s", c.name, d.Action, d.Reason, c.action, c.reason)
		}
		if d.Action == ActionUpdate && d.Target.Seq <= c.cur.Seq {
			t.Errorf("%s: seq %d not above the stored %d", c.name, d.Target.Seq, c.cur.Seq)
		}
	}
}

func TestSeqAlwaysRisesAndANewerProcessWins(t *testing.T) {
	if got := NextSeq(5, now); got != uint64(now.UnixMilli()) {
		t.Fatalf("the clock leads an old seq: %d", got)
	}
	ahead := uint64(now.UnixMilli()) + 10
	if got := NextSeq(ahead, now); got != ahead+1 {
		t.Fatalf("a stored seq ahead of the clock is still exceeded: %d", got)
	}
	// a draining process a second behind its successor computes a lower seq, which the book refuses
	if NextSeq(0, now.Add(-time.Second)) >= NextSeq(0, now) {
		t.Fatal("the older clock must lose")
	}
}

func TestParamsAreValidated(t *testing.T) {
	if err := params().Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []func(*Params){
		func(p *Params) { p.TTL = 5 * time.Second },
		func(p *Params) { p.RefreshAfter = p.TTL },
		func(p *Params) { p.RefreshAfter = 0 },
		func(p *Params) { p.RepriceTicks = 0 },
		func(p *Params) { p.MaxDivergenceBPS = 0 },
		func(p *Params) { p.CutoffBeforeLastTrade = -time.Second },
		func(p *Params) { p.BidCurve = 0 },
	}
	for i, mutate := range bad {
		p := params()
		mutate(&p)
		if p.Validate() == nil {
			t.Errorf("bad params %d accepted", i)
		}
	}
}

func TestTheCurveIsParsedWithTheContractsRules(t *testing.T) {
	got, err := ParseCurve(" 200@0, 400@50 ,600@50")
	if err != nil || len(got) != 3 || got[1] != (Region{Lots: 400, OffsetTicks: 50}) {
		t.Fatalf("%+v %v", got, err)
	}
	for _, bad := range []string{"", "200", "0@0", "200@50,200@40", "x@1", "200@-1",
		"1@0,1@1,1@2,1@3,1@4,1@5,1@6,1@7,1@8,1@9,1@10"} {
		if _, err := ParseCurve(bad); err == nil {
			t.Errorf("curve %q accepted", bad)
		}
	}
}
