// Package pricing prices European options the way MarginEngine v2 marks them (risk-core
// MarginEngineOptionHelper._getMarkToMarket, lyra-utils Black76): Black-76 on the forward, no discounting, time to
// expiry in seconds over a 365-day year. A quote built on this agrees with the mark the engine margins the
// position at, so a writer's requirement is what the bot expects.
package pricing

import "math"

// SecondsPerYear is Black76.annualise's year: 365 days.
const SecondsPerYear = 31_536_000.0

// Option is one strike and side at a forward, a vol and a time to expiry.
type Option struct {
	Forward float64 // USDC per base, at the option's expiry
	Strike  float64 // USDC per base
	Vol     float64 // annualised, e.g. 0.40
	Seconds float64 // to expiry; 0 or less is expired
	Call    bool
}

// Years is the engine's annualisation: seconds / 365 days.
func (o Option) Years() float64 {
	if o.Seconds <= 0 {
		return 0
	}
	return o.Seconds / SecondsPerYear
}

func phi(x float64) float64 { return 0.5 * math.Erfc(-x/math.Sqrt2) }

// d1d2 are Black-76's d1, d2. ok is false when the option has no time value to price (expired, zero vol, or a
// non-positive forward or strike): Price then returns intrinsic value.
func (o Option) d1d2() (d1, d2 float64, ok bool) {
	t := o.Years()
	if t <= 0 || o.Vol <= 0 || o.Forward <= 0 || o.Strike <= 0 {
		return 0, 0, false
	}
	sd := o.Vol * math.Sqrt(t)
	d1 = (math.Log(o.Forward/o.Strike) + 0.5*sd*sd) / sd
	return d1, d1 - sd, true
}

func (o Option) intrinsic() float64 {
	if o.Call {
		return math.Max(0, o.Forward-o.Strike)
	}
	return math.Max(0, o.Strike-o.Forward)
}

// Price is the premium per unit of the base, in USDC: F N(d1) - K N(d2) for a call, K N(-d2) - F N(-d1) for a put.
func (o Option) Price() float64 {
	d1, d2, ok := o.d1d2()
	if !ok {
		return o.intrinsic()
	}
	if o.Call {
		return math.Max(0, o.Forward*phi(d1)-o.Strike*phi(d2))
	}
	return math.Max(0, o.Strike*phi(-d2)-o.Forward*phi(-d1))
}

// Delta is dPrice/dForward: N(d1) for a call, N(d1) - 1 for a put. A held position's delta in the base is
// Delta x balance.
func (o Option) Delta() float64 {
	d1, _, ok := o.d1d2()
	if !ok {
		switch {
		case o.Call && o.Forward > o.Strike:
			return 1
		case !o.Call && o.Forward < o.Strike:
			return -1
		}
		return 0
	}
	if o.Call {
		return phi(d1)
	}
	return phi(d1) - 1
}

// Vega is dPrice/dVol, per 1.00 of vol (so per vol point it is Vega / 100): F N'(d1) sqrt(t), the same for a call
// and a put.
func (o Option) Vega() float64 {
	d1, _, ok := o.d1d2()
	if !ok {
		return 0
	}
	return o.Forward * math.Exp(-0.5*d1*d1) / math.Sqrt(2*math.Pi) * math.Sqrt(o.Years())
}

// AtVol is the same option at another vol: the bid and ask of a quote spread in vol points.
func (o Option) AtVol(vol float64) Option {
	o.Vol = vol
	return o
}
