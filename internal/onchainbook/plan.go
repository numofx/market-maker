// Package onchainbook quotes EURC-30NOV26 on risk-core's OnchainBook (exchange repo,
// contracts/risk-core/src/periphery/OnchainBook.sol; spec docs/eurc-usdc-futures-spec.md, Part B).
//
// The book does not hold orders. A maker registers a curve shape once (regions of lots at tick offsets
// from the best level, setCurve) and then keeps one spline per market: a mid as an offset in ticks from
// the index the book reads at each take, a half-spread, and on-chain skew and refill. Repricing is one
// updateSplines transaction that moves the offset; the book prices every take against the index of that
// moment, so the offset only has to track the forward's basis to the index, which moves slowly.
//
// Self-priced (v2 books, spec C4.2, Params.SelfPriced): the spline's mid is an absolute price in ticks, which the book
// quotes as sent, with no index read, until the spline's ttl (capped by the market's maxSelfPricedTtl) lapses. The
// maker then carries the staleness: a short ttl, refreshed every cycle. The book still band-checks every fill
// against the index, so a stale index stops self-priced quotes too.
//
// Inventory cap (Params.MaxPositionLots): the book has no per-maker position limit, and every spline update resets
// what was taken from each side, so a curve is a size per update, not a total. Each cycle reads the account's
// position; a side whose whole curve, filled, would take the position past the cap is quoted on the park curve
// instead: one lot ParkOffsetTicks out, far outside the index band, which the book never fills (its walk stops there).
// The position can still pass the cap by fills landing between a cycle's read and its update: at most one curve.
//
// This file is the decision, with no chain access: given what was read, what should the spline be and
// is a transaction worth sending.
package onchainbook

import (
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// Region is one step of a curve: Lots at OffsetTicks from the side's best level (OnchainBook.Region).
type Region struct {
	Lots        uint32
	OffsetTicks uint32
}

// Spline mirrors OnchainBook.Spline as read from splines(marketId, accountId).
type Spline struct {
	OffsetTicks         int32
	HalfSpreadTicks     uint32
	BidCurve            uint16
	AskCurve            uint16
	SkewTicksPerLot     uint32
	MaxSkewTicks        uint32
	RefillLotsPerSecond uint32
	UpdatedAt           time.Time
	TTL                 uint32
	Seq                 uint64
	Active              bool
	// Owner is the account owner that wrote it: after a transfer of the account the book stops quoting it.
	Owner common.Address
	// SelfPriced: OffsetTicks is the mid itself in ticks, not an offset from the index (v2 books only).
	SelfPriced bool
}

// Live is whether the book would quote it at now: active and within its ttl. The book also requires the account's
// current owner to be Owner; Maker.Cycle marks a spline written by another owner as inactive before deciding.
func (s Spline) Live(now time.Time) bool {
	return s.Active && !now.After(s.UpdatedAt.Add(time.Duration(s.TTL)*time.Second))
}

// Params is the operator's configuration of the spline.
type Params struct {
	HalfSpreadTicks     uint32
	BidCurve            uint16
	AskCurve            uint16
	SkewTicksPerLot     uint32
	MaxSkewTicks        uint32
	RefillLotsPerSecond uint32
	TTL                 time.Duration
	// RepriceTicks is the smallest move of the target offset worth a transaction.
	RepriceTicks int64
	// RefreshAfter re-sends an unchanged spline once it is this old, well inside TTL, so a slow block
	// never lets it lapse.
	RefreshAfter time.Duration
	// CutoffBeforeLastTrade takes the spline down this long before the series' last trade.
	CutoffBeforeLastTrade time.Duration
	// MaxDivergenceBPS cancels when the Data Streams index and the Chainlink push reference disagree by
	// more: the index is DEX-weighted and the book cannot tell a pool skew from a move (spec B6, B14a).
	MaxDivergenceBPS float64
	// SelfPriced sends the forward itself as the mid (round(forward / tick) ticks) rather than its offset from the
	// index. The quote is then only as fresh as the last update: keep TTL short and RefreshAfter under the poll.
	SelfPriced bool
	// MaxPositionLots caps the account's position, long or short, in lots; 0 is no cap. CurveLots is one side's
	// whole curve in lots, and ParkCurve the curve id a side is moved to at the cap (ParkCurveRegions).
	MaxPositionLots uint64
	CurveLots       uint64
	ParkCurve       uint16
}

// ParkOffsetTicks is the park curve's one region: 500,000 ticks behind the best level, 0.5 USDC at a 1e-6 tick, about
// 45% of a 1.10 price. Preflight refuses a market whose index band is not well inside it.
const ParkOffsetTicks = 500_000

// ParkCurveRegions is the curve a side quotes at the cap: one lot nobody can take.
func ParkCurveRegions() []Region { return []Region{{Lots: 1, OffsetTicks: ParkOffsetTicks}} }

func (p Params) Validate() error {
	switch {
	case p.TTL < 10*time.Second:
		return fmt.Errorf("ttl %s: at least 10s", p.TTL)
	case p.RefreshAfter <= 0 || p.RefreshAfter >= p.TTL:
		return fmt.Errorf("refresh after %s must be positive and under the ttl %s", p.RefreshAfter, p.TTL)
	case p.RepriceTicks <= 0:
		return fmt.Errorf("reprice ticks %d must be positive", p.RepriceTicks)
	case p.MaxDivergenceBPS <= 0:
		return fmt.Errorf("max divergence %v bps must be positive", p.MaxDivergenceBPS)
	case p.CutoffBeforeLastTrade < 0:
		return fmt.Errorf("cutoff %s must not be negative", p.CutoffBeforeLastTrade)
	case p.BidCurve == 0 || p.AskCurve == 0:
		return fmt.Errorf("curve ids must be set")
	}
	if p.MaxPositionLots > 0 {
		switch {
		case p.ParkCurve == 0 || p.ParkCurve == p.BidCurve || p.ParkCurve == p.AskCurve:
			return fmt.Errorf("park curve id %d must be set and differ from the quoting curves", p.ParkCurve)
		case p.CurveLots == 0:
			return fmt.Errorf("curve lots must be set with a position cap")
		case p.CurveLots > p.MaxPositionLots:
			return fmt.Errorf("one side's curve (%d lots) is over the position cap (%d): a flat account could not quote it", p.CurveLots, p.MaxPositionLots)
		}
	}
	return nil
}

// SendLatency is the budget for an update to land after the cycle that decided it: a Base block is 2s, plus the
// estimate and the receipt poll.
const SendLatency = 3 * time.Second

// CheckCadence refuses a self-priced configuration whose quote can lapse between cycles. A cycle that finds the
// spline just under RefreshAfter old sends nothing; the next comes a poll later and its update lands SendLatency
// after that, so RefreshAfter + poll + SendLatency must fit in the ttl. (Pegged splines live for an hour and are not
// held to it.)
func CheckCadence(p Params, poll time.Duration) error {
	if !p.SelfPriced {
		return nil
	}
	if worst := p.RefreshAfter + poll + SendLatency; worst > p.TTL {
		return fmt.Errorf("self-priced: refresh %s + poll %s + send %s = %s, over the ttl %s: the quote would lapse between cycles",
			p.RefreshAfter, poll, SendLatency, worst, p.TTL)
	}
	return nil
}

// Inputs is one cycle's reads. A nil price is one that could not be read: the feeds revert when stale.
type Inputs struct {
	Now           time.Time
	Index         *big.Int // EurcIndexFeed.getSpot, USDC per EURC, 18 dp
	Forward       *big.Int // the series' forward (LyraForwardFeed.getForwardPrice(expiry)), 18 dp
	Reference     *big.Int // market 2's Chainlink push feed, USD per EURC, 18 dp
	TickSize      *big.Int // the book market's tick, 18 dp
	LastTradeTime time.Time
	// PositionLots is the account's position in the series, in lots (negative short); nil when it could not be read.
	// Read only with a cap.
	PositionLots *int64
}

type Action int

const (
	ActionNone Action = iota
	ActionUpdate
	ActionCancel
)

func (a Action) String() string {
	return [...]string{"none", "update", "cancel"}[a]
}

// Decision is what to do this cycle, and why.
type Decision struct {
	Action Action
	Reason string
	Target Spline // for ActionUpdate: the spline to send, Seq included
}

// Decide is the whole policy. Every way it cannot price safely takes the spline down (cancel when it is
// live, nothing when it is already down); otherwise it sends an update only when the target moved by
// RepriceTicks, the configuration changed, the spline is down, or it is due a refresh.
func Decide(p Params, in Inputs, cur Spline) Decision {
	down := func(reason string) Decision {
		if cur.Active {
			return Decision{Action: ActionCancel, Reason: reason}
		}
		return Decision{Action: ActionNone, Reason: reason}
	}
	if !in.Now.Before(in.LastTradeTime.Add(-p.CutoffBeforeLastTrade)) {
		return down("cutoff_before_last_trade")
	}
	if in.Index == nil || in.Index.Sign() <= 0 {
		return down("index_unreadable")
	}
	if in.Forward == nil || in.Forward.Sign() <= 0 {
		return down("forward_unreadable")
	}
	if in.Reference == nil || in.Reference.Sign() <= 0 {
		return down("reference_unreadable")
	}
	if divergenceBPS(in.Index, in.Reference) > p.MaxDivergenceBPS {
		return down("index_reference_divergence")
	}
	var offset int32
	var ok bool
	if p.SelfPriced {
		offset, ok = MidTicks(in.Forward, in.TickSize)
	} else {
		offset, ok = OffsetTicks(in.Forward, in.Index, in.TickSize)
	}
	if !ok {
		return down("offset_out_of_range")
	}
	bidCurve, askCurve := p.BidCurve, p.AskCurve
	if p.MaxPositionLots > 0 {
		if in.PositionLots == nil {
			return down("position_unreadable")
		}
		pos, side, limit := *in.PositionLots, int64(p.CurveLots), int64(p.MaxPositionLots)
		if pos+side > limit { // the bid buys: filled, it adds to a long
			bidCurve = p.ParkCurve
		}
		if pos-side < -limit { // the ask sells: filled, it adds to a short
			askCurve = p.ParkCurve
		}
	}

	target := Spline{
		OffsetTicks:         offset,
		HalfSpreadTicks:     p.HalfSpreadTicks,
		BidCurve:            bidCurve,
		AskCurve:            askCurve,
		SkewTicksPerLot:     p.SkewTicksPerLot,
		MaxSkewTicks:        p.MaxSkewTicks,
		RefillLotsPerSecond: p.RefillLotsPerSecond,
		TTL:                 uint32(p.TTL / time.Second),
		Seq:                 NextSeq(cur.Seq, in.Now),
		Active:              true,
		SelfPriced:          p.SelfPriced,
	}
	reason := ""
	switch {
	case !cur.Live(in.Now):
		reason = "down"
	case cur.HalfSpreadTicks != target.HalfSpreadTicks || cur.BidCurve != target.BidCurve ||
		cur.AskCurve != target.AskCurve || cur.SkewTicksPerLot != target.SkewTicksPerLot ||
		cur.MaxSkewTicks != target.MaxSkewTicks || cur.RefillLotsPerSecond != target.RefillLotsPerSecond ||
		cur.TTL != target.TTL || cur.SelfPriced != target.SelfPriced:
		reason = "config_changed"
	case abs64(int64(target.OffsetTicks)-int64(cur.OffsetTicks)) >= p.RepriceTicks:
		reason = "repriced"
	case in.Now.Sub(cur.UpdatedAt) >= p.RefreshAfter:
		reason = "refresh"
	default:
		return Decision{Action: ActionNone, Reason: "unchanged"}
	}
	return Decision{Action: ActionUpdate, Reason: reason, Target: target}
}

// OffsetTicks is round((forward - index) / tick), the mid's distance from the index, if it fits an int32.
func OffsetTicks(forward, index, tick *big.Int) (int32, bool) {
	if tick == nil || tick.Sign() <= 0 {
		return 0, false
	}
	diff := new(big.Int).Sub(forward, index)
	q, r := new(big.Int).QuoRem(diff, tick, new(big.Int))
	// round half away from zero
	if new(big.Int).Mul(new(big.Int).Abs(r), big.NewInt(2)).Cmp(tick) >= 0 {
		if diff.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	if !q.IsInt64() || q.Int64() > math.MaxInt32 || q.Int64() < math.MinInt32 {
		return 0, false
	}
	return int32(q.Int64()), true
}

// MidTicks is round(forward / tick), a self-priced mid, if it is positive and fits an int32 (the book refuses a
// self-priced mid at or below 0: OB_InvalidMid).
func MidTicks(forward, tick *big.Int) (int32, bool) {
	q, ok := OffsetTicks(forward, big.NewInt(0), tick)
	return q, ok && q > 0
}

// NextSeq is strictly above the stored seq and at least the wall clock in milliseconds, so a newer
// process's updates supersede an older one's without reading first: the book refuses a seq not above
// the stored one (OB_StaleSeq), so a draining container cannot overwrite its successor.
func NextSeq(stored uint64, now time.Time) uint64 {
	ms := uint64(now.UnixMilli())
	if stored+1 > ms {
		return stored + 1
	}
	return ms
}

func divergenceBPS(a, b *big.Int) float64 {
	af, _ := new(big.Float).SetInt(a).Float64()
	bf, _ := new(big.Float).SetInt(b).Float64()
	return math.Abs(af/bf-1) * 10_000
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
