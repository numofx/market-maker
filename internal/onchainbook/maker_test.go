package onchainbook

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// fakeBook stores one spline and applies updates and cancels as the contract does (seq must rise).
type fakeBook struct {
	spline                      Spline
	index, forward, reference   *big.Int
	updates, cancels, priceRead int
	failUpdate                  error
}

func (f *fakeBook) Prices(context.Context, Feeds, uint64) (*big.Int, *big.Int, *big.Int, []error) {
	f.priceRead++
	return f.index, f.forward, f.reference, nil
}

func (f *fakeBook) Spline(context.Context) (Spline, error) { return f.spline, nil }

func (f *fakeBook) Update(_ context.Context, s Spline) (common.Hash, error) {
	if f.failUpdate != nil {
		return common.Hash{}, f.failUpdate
	}
	if s.Seq <= f.spline.Seq {
		return common.Hash{}, &RevertError{Name: "OB_StaleSeq"}
	}
	f.updates++
	s.UpdatedAt = time.Unix(int64(s.Seq/1000), 0)
	s.Owner = maker
	f.spline = s
	return common.Hash{1}, nil
}

func (f *fakeBook) Cancel(context.Context) (common.Hash, error) {
	f.cancels++
	f.spline.Active = false
	return common.Hash{2}, nil
}

var maker = common.HexToAddress("0x3a4e")

func newMaker(book *fakeBook, clock *time.Time) *Maker {
	in := inputs()
	book.index, book.forward, book.reference = in.Index, in.Forward, in.Reference
	return &Maker{Book: book, Params: params(), Market: Market{TickSize: tick, LastTradeTime: in.LastTradeTime}, Owner: maker,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return *clock }}
}

func TestACycleSendsThenHoldsUntilAMoveOrARefresh(t *testing.T) {
	clock := now
	book := &fakeBook{}
	m := newMaker(book, &clock)
	if d, err := m.Cycle(context.Background()); err != nil || d.Action != ActionUpdate || book.updates != 1 {
		t.Fatalf("first cycle: %+v %v updates=%d", d, err, book.updates)
	}
	clock = clock.Add(5 * time.Second)
	if d, _ := m.Cycle(context.Background()); d.Action != ActionNone || book.updates != 1 {
		t.Fatalf("an unchanged spline is not resent: %+v", d)
	}
	book.forward = new(big.Int).Add(book.forward, new(big.Int).Mul(big.NewInt(25), tick))
	if d, _ := m.Cycle(context.Background()); d.Reason != "repriced" || book.updates != 2 {
		t.Fatalf("a 25-tick move reprices: %+v", d)
	}
	clock = clock.Add(31 * time.Minute)
	if d, _ := m.Cycle(context.Background()); d.Reason != "refresh" || book.updates != 3 {
		t.Fatalf("refresh: %+v", d)
	}
}

func TestDryRunSendsNothing(t *testing.T) {
	clock := now
	book := &fakeBook{}
	m := newMaker(book, &clock)
	m.DryRun = true
	if d, err := m.Cycle(context.Background()); err != nil || d.Action != ActionUpdate {
		t.Fatalf("%+v %v", d, err)
	}
	book.spline = live(495, now)
	book.reference = nil
	if d, _ := m.Cycle(context.Background()); d.Action != ActionCancel {
		t.Fatalf("%+v", d)
	}
	if book.updates != 0 || book.cancels != 0 {
		t.Fatalf("dry run sent %d updates, %d cancels", book.updates, book.cancels)
	}
	if err := m.Shutdown(context.Background()); err != nil || book.cancels != 0 {
		t.Fatal("dry run cancels nothing at shutdown")
	}
}

func TestALiveSplineComesDownWhenTheReferenceDiverges(t *testing.T) {
	clock := now
	book := &fakeBook{}
	m := newMaker(book, &clock)
	_, _ = m.Cycle(context.Background())
	book.index, book.reference = e18(1.1), e18(1.1*1.006)
	if d, _ := m.Cycle(context.Background()); d.Action != ActionCancel || book.cancels != 1 || book.spline.Active {
		t.Fatalf("%+v cancels=%d", d, book.cancels)
	}
	if d, _ := m.Cycle(context.Background()); d.Action != ActionNone || book.cancels != 1 {
		t.Fatal("an already-down spline is not cancelled again")
	}
}

// A rollout (CLAUDE.md, exchange repo): the old task keeps running for seconds after the new one starts. The new one writes
// with a later clock; the old one must stop at its next cycle, and must not cancel the new one's spline.
func TestAnOlderProcessStepsAsideAndDoesNotCancelItsSuccessor(t *testing.T) {
	clock := now
	book := &fakeBook{}
	old := newMaker(book, &clock)
	if _, err := old.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(2 * time.Second)
	successor := newMaker(book, &clock)
	if d, err := successor.Cycle(context.Background()); err != nil || d.Action != ActionUpdate {
		t.Fatalf("a new process takes over at once: %+v %v", d, err)
	}
	if _, err := old.Cycle(context.Background()); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("the old process must stop: %v", err)
	}
	if err := old.Shutdown(context.Background()); err != nil || book.cancels != 0 || !book.spline.Active {
		t.Fatalf("the old process's shutdown cancelled its successor's spline (cancels=%d)", book.cancels)
	}
	if err := successor.Shutdown(context.Background()); err != nil || book.cancels != 1 || book.spline.Active {
		t.Fatal("the owner's shutdown cancels its own spline")
	}
}

func TestAStaleSeqRevertIsSupersession(t *testing.T) {
	clock := now
	book := &fakeBook{failUpdate: &RevertError{Name: "OB_StaleSeq"}}
	m := newMaker(book, &clock)
	if _, err := m.Cycle(context.Background()); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("OB_StaleSeq: %v", err)
	}
	book.failUpdate = errors.New("rpc down")
	if _, err := m.Cycle(context.Background()); err == nil || errors.Is(err, ErrSuperseded) {
		t.Fatalf("another failure is an error, not a hand-off: %v", err)
	}
}

func TestAHeldDownSplineSaysWhyOnce(t *testing.T) {
	clock := now
	book := &fakeBook{}
	m := newMaker(book, &clock)
	var buf strings.Builder
	m.Log = slog.New(slog.NewTextHandler(&buf, nil)) // info level
	book.reference = nil
	for i := 0; i < 3; i++ {
		_, _ = m.Cycle(context.Background())
	}
	if n := strings.Count(buf.String(), "reason=reference_unreadable"); n != 1 {
		t.Fatalf("the reason is logged once at info, got %d:\n%s", n, buf.String())
	}
}

// A spline written by the account's previous owner is ignored by the book: the bot re-sends rather than trusting it.
func TestASplineFromAPreviousOwnerIsTreatedAsDown(t *testing.T) {
	clock := now
	book := &fakeBook{}
	m := newMaker(book, &clock)
	if _, err := m.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(5 * time.Second)
	book.spline.Owner = common.HexToAddress("0x01d")
	if d, _ := m.Cycle(context.Background()); d.Action != ActionUpdate || d.Reason != "down" {
		t.Fatalf("a spline the book ignores must be re-sent: %+v", d)
	}
	if book.spline.Owner != maker {
		t.Fatal("re-sent under the current owner")
	}
}
