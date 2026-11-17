package onchainbook

import (
	"context"
	"errors"
	"math/big"
	"testing"
)

// capped is the configuration of a maker capped at 100 lots, quoting a 30-lot side, parking on curve 2.
func capped() Params {
	p := params()
	p.MaxPositionLots, p.CurveLots, p.ParkCurve = 100, 30, 2
	return p
}

func at(pos int64) Inputs {
	in := inputs()
	in.PositionLots = &pos
	return in
}

// A side is parked exactly when its whole curve, filled, would take the position past the cap: both edges.
func TestASideIsParkedWhenAFullFillWouldPassTheCap(t *testing.T) {
	cases := []struct {
		pos      int64
		bid, ask uint16
	}{
		{0, 1, 1},
		{70, 1, 1},   // 70 + 30 = 100: at the cap, not past it
		{71, 2, 1},   // 101: the bid (which buys) parks; the ask still sells it down
		{100, 2, 1},  // at the cap
		{150, 2, 1},  // past it (fills between a read and an update): still only reduces
		{-70, 1, 1},  // -70 - 30 = -100
		{-71, 1, 2},  // the ask (which sells) parks
		{-150, 1, 2}, // past it the other way
	}
	for _, c := range cases {
		d := Decide(capped(), at(c.pos), Spline{})
		if d.Action != ActionUpdate || d.Target.BidCurve != c.bid || d.Target.AskCurve != c.ask {
			t.Errorf("position %d: bid curve %d ask curve %d (%s), want %d and %d", c.pos, d.Target.BidCurve,
				d.Target.AskCurve, d.Reason, c.bid, c.ask)
		}
	}
}

// Parking and unparking is a configuration change: it sends at once, not at the next refresh.
func TestReachingAndLeavingTheCapSendsAtOnce(t *testing.T) {
	p := capped()
	first := Decide(p, at(0), Spline{})
	cur := first.Target
	cur.UpdatedAt, cur.Owner = now, maker
	if d := Decide(p, at(0), cur); d.Action != ActionNone {
		t.Fatalf("unchanged at 0: %+v", d)
	}
	if d := Decide(p, at(71), cur); d.Action != ActionUpdate || d.Reason != "config_changed" || d.Target.BidCurve != 2 {
		t.Fatalf("at 71 the bid parks now: %+v", d)
	}
	parked := cur
	parked.BidCurve = 2
	if d := Decide(p, at(40), parked); d.Action != ActionUpdate || d.Reason != "config_changed" || d.Target.BidCurve != 1 {
		t.Fatalf("back at 40 the bid quotes again now: %+v", d)
	}
}

// With a cap, a position that cannot be read cannot be capped: the spline comes down, as for a stale price.
func TestAnUnreadablePositionTakesTheSplineDown(t *testing.T) {
	in := inputs() // PositionLots nil
	if d := Decide(capped(), in, live(495, now)); d.Action != ActionCancel || d.Reason != "position_unreadable" {
		t.Fatalf("%+v", d)
	}
	if d := Decide(params(), in, Spline{}); d.Action != ActionUpdate {
		t.Fatalf("no cap: the position is not needed: %+v", d)
	}
}

func TestTheCapsParamsAreValidated(t *testing.T) {
	for name, mut := range map[string]func(*Params){
		"no park curve":           func(p *Params) { p.ParkCurve = 0 },
		"park is the quote curve": func(p *Params) { p.ParkCurve = 1 },
		"no curve lots":           func(p *Params) { p.CurveLots = 0 },
		"a side over the cap":     func(p *Params) { p.CurveLots = 101 },
	} {
		p := capped()
		mut(&p)
		if p.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := capped().Validate(); err != nil {
		t.Fatal(err)
	}
	p := params() // no cap: the park curve is not needed
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
}

// The cycle reads the position only with a cap, and passes it through.
func TestTheCycleReadsThePositionForTheCap(t *testing.T) {
	clock := now
	book := &fakeBook{position: -80}
	m := newMaker(book, &clock)
	m.Params = capped()
	d, err := m.Cycle(context.Background())
	if err != nil || d.Action != ActionUpdate || d.Target.AskCurve != 2 || d.Target.BidCurve != 1 {
		t.Fatalf("short 80 of 100: the ask parks: %+v %v", d, err)
	}
	book.positionErr = errors.New("rpc down")
	if d, _ := m.Cycle(context.Background()); d.Action != ActionCancel || d.Reason != "position_unreadable" {
		t.Fatalf("%+v", d)
	}
}

// The park level must sit at least twice the band from the mid. The live series: band 2.79%, index 1.10, tick 1e-6.
func TestTheParkCurveMustBeOutsideTheBand(t *testing.T) {
	tk := big.NewInt(1e12)
	index := e18(1.1)
	if !parkOutsideBand(big.NewInt(27_924_313_822_751_322), index, tk, 880) {
		t.Fatal("the live band is well inside 500,000 ticks")
	}
	// 500,880 ticks = 0.50088: twice a band b at 1.10 is 2.2 b, so b up to ~22.77% passes and 23% does not.
	if !parkOutsideBand(e18(0.2276), index, tk, 880) || parkOutsideBand(e18(0.23), index, tk, 880) {
		t.Fatal("the edge")
	}
}
