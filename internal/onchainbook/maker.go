package onchainbook

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// Book is what the Maker needs from the chain; *Chain implements it.
type Book interface {
	Prices(ctx context.Context, f Feeds, expiry uint64) (index, forward, reference *big.Int, errs []error)
	Spline(ctx context.Context) (Spline, error)
	Update(ctx context.Context, s Spline) (common.Hash, error)
	Cancel(ctx context.Context) (common.Hash, error)
}

// ErrSuperseded is returned once another process has written the spline after this one: the stored seq is
// above anything this process sent. The newer process owns the spline; this one stops, and does not cancel.
var ErrSuperseded = errors.New("the spline was written by another process since this one's last update: stopping")

type Maker struct {
	Book   Book
	Params Params
	Feeds  Feeds
	Market Market
	DryRun bool
	Log    *slog.Logger
	Now    func() time.Time

	// lastSent is the seq of this process's last landed update; 0 before its first.
	lastSent uint64
	// lastReason is the previous cycle's reason: a change is logged at info, so a spline held down (divergence,
	// cutoff, a stale feed) is visible without debug logging, once, not every cycle.
	lastReason string
}

func (m *Maker) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Cycle reads, decides and acts once.
func (m *Maker) Cycle(ctx context.Context) (Decision, error) {
	cur, err := m.Book.Spline(ctx)
	if err != nil {
		return Decision{}, err
	}
	// Hand-off: a newer process's seq is from a later clock, so once anything above our last write is stored,
	// that process owns the spline. A process that has not written yet (lastSent 0) takes over instead.
	if m.lastSent != 0 && cur.Seq > m.lastSent {
		return Decision{}, ErrSuperseded
	}
	index, forward, reference, errs := m.Book.Prices(ctx, m.Feeds, m.Market.Expiry)
	for _, e := range errs {
		m.Log.Warn("price read failed", "error", e)
	}
	in := Inputs{
		Now: m.now(), Index: index, Forward: forward, Reference: reference,
		TickSize: m.Market.TickSize, LastTradeTime: m.Market.LastTradeTime,
	}
	d := Decide(m.Params, in, cur)
	// Claim: a process that has not written yet sends once even when the stored spline needs no change, so the
	// seq it stores marks it as the owner. Otherwise its predecessor keeps the spline and cancels it on its way
	// out, and nothing quotes until this process's next cycle notices.
	if m.lastSent == 0 && d.Action == ActionNone && d.Reason == "unchanged" {
		d = Decide(m.Params, in, Spline{Seq: cur.Seq})
		d.Reason = "claim"
	}
	attrs := []any{"action", d.Action.String(), "reason", d.Reason, "stored_seq", cur.Seq, "stored_offset", cur.OffsetTicks,
		"stored_live", cur.Live(m.now())}
	if d.Action == ActionUpdate {
		attrs = append(attrs, "offset_ticks", d.Target.OffsetTicks, "half_spread_ticks", d.Target.HalfSpreadTicks, "seq", d.Target.Seq)
	}
	changed := d.Reason != m.lastReason
	m.lastReason = d.Reason
	if d.Action == ActionNone {
		if changed {
			m.Log.Info("spline state", attrs...)
		} else {
			m.Log.Debug("spline", attrs...)
		}
		return d, nil
	}
	if m.DryRun {
		m.Log.Info("spline (dry run: nothing sent)", attrs...)
		return d, nil
	}
	var hash common.Hash
	if d.Action == ActionUpdate {
		hash, err = m.Book.Update(ctx, d.Target)
		if err == nil {
			m.lastSent = d.Target.Seq
		} else if IsRevert(err, "OB_StaleSeq") {
			return d, fmt.Errorf("%w (%v)", ErrSuperseded, err)
		}
	} else {
		hash, err = m.Book.Cancel(ctx)
	}
	if err != nil {
		m.Log.Error("spline transaction failed", append(attrs, "tx", hash.Hex(), "error", err)...)
		return d, err
	}
	m.Log.Info("spline", append(attrs, "tx", hash.Hex())...)
	return d, nil
}

// Shutdown cancels the spline only if it is still this process's: a successor that has already written must
// keep quoting (a draining container's cancel would otherwise take down the live one).
func (m *Maker) Shutdown(ctx context.Context) error {
	if m.DryRun || m.lastSent == 0 {
		return nil
	}
	cur, err := m.Book.Spline(ctx)
	if err != nil {
		return err
	}
	if !cur.Active || cur.Seq != m.lastSent {
		m.Log.Info("shutdown: spline left as is", "stored_seq", cur.Seq, "last_sent", m.lastSent, "active", cur.Active)
		return nil
	}
	hash, err := m.Book.Cancel(ctx)
	if err != nil {
		return err
	}
	m.Log.Info("shutdown: spline cancelled", "tx", hash.Hex())
	return nil
}
