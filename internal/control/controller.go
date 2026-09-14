// Package control is the operator's runtime handle on the bot: kill, pause, pull a side, lean the
// quotes. The terminal sidecar drives it over loopback HTTP (server.go); the quoting loop consults the
// Controller every cycle and wakes on every change.
package control

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/state"
)

type State string

const (
	StateRunning State = "RUNNING"
	StatePaused  State = "PAUSED"
	StateKilled  State = "KILLED"
	StateHalted  State = "HALTED"
)

const (
	SourceManual     = "manual"
	SourceRisk       = "risk"
	SourceStaleFeed  = "stale_feed"
	SourceChain      = "chain"
	SourceKillSwitch = "kill_switch"
	SourceStartup    = "startup"
)

// Halt reasons the bot records for operator stops. They are not risk halts, and a state computed
// from them must never read HALTED -- in particular in the moment after a resume and before the next
// cycle has cleared the halt.
const (
	HaltReasonKillSwitchFile = "kill switch active"
	HaltReasonControlKill    = "control kill active"
	HaltReasonOperatorPause  = "operator pause active"
	HaltReasonControlPause   = "control pause active"
)

// Bounds on /control/adjust. Wide enough for an operator leaning through a move, narrow enough that
// a typo cannot quote the book a few percent away or at a size the caps were never tuned for.
const (
	MaxMidShiftBPS  = 200
	MaxSpreadAddBPS = 500
	MaxSizeMult     = 5
)

const maxActions = 50

// Adjust is the operator's lean on top of config. See strategy.Overrides for where each applies.
type Adjust struct {
	MidShiftBPS  float64 `json:"mid_shift_bps"`
	SpreadAddBPS float64 `json:"spread_add_bps"`
	SizeMult     float64 `json:"size_mult"`
}

// AdjustPatch is a partial update: nil fields are left as they are.
type AdjustPatch struct {
	MidShiftBPS  *float64 `json:"mid_shift_bps"`
	SpreadAddBPS *float64 `json:"spread_add_bps"`
	SizeMult     *float64 `json:"size_mult"`
	Reason       string   `json:"reason"`
}

// Action is one place or cancel attempt, as the terminal's activity feed shows it.
type Action struct {
	At      time.Time `json:"at"`
	Action  string    `json:"action"`
	Side    string    `json:"side"`
	Price   float64   `json:"price"`
	Size    float64   `json:"size"`
	OrderID string    `json:"order_id"`
	Reason  string    `json:"reason"`
	DryRun  bool      `json:"dry_run"`
	Error   string    `json:"error"`
}

// Status is a consistent copy of the controller's overrides.
type Status struct {
	Killed      bool
	KillReason  string
	KilledAt    time.Time
	Paused      bool
	PauseReason string
	PausedAt    time.Time
	BidEnabled  bool
	AskEnabled  bool
	Adjust      Adjust
}

// ErrKilled is a resume that would silently undo a kill.
var ErrKilled = errors.New("killed; resume requires clear_kill=true")

// Controller holds the runtime overrides. Every method is safe for concurrent use, and every read
// method is safe on a nil *Controller (it reports "no overrides"), so a bot built without one -- every
// existing test -- behaves exactly as before.
type Controller struct {
	mu          sync.Mutex
	killed      bool
	killReason  string
	killedAt    time.Time
	paused      bool
	pauseReason string
	pausedAt    time.Time
	bidEnabled  bool
	askEnabled  bool
	adjust      Adjust
	// dirty is set by any change and taken by the next cycle, which then skips the quote-refresh
	// throttle: an adjust or a restored side must reach the book on the cycle it wakes, not up to
	// MM_QUOTE_REFRESH_INTERVAL_MS later.
	dirty bool

	actions     [maxActions]Action
	actionsNext int
	actionsLen  int

	halfSpreadBPS float64
	createdAt     time.Time
	store         *state.Store
	wake          chan struct{}
}

// NewController restores kill/pause from the state file and installs itself as the store's overlay,
// so every subsequent write of MM_STATE_FILE -- the loop's or its own -- carries the current kill and
// pause. halfSpreadBPS is the configured half spread, which bounds how negative spread_add_bps may go.
func NewController(store *state.Store, halfSpreadBPS float64) (*Controller, error) {
	c := &Controller{
		bidEnabled:    true,
		askEnabled:    true,
		adjust:        Adjust{SizeMult: 1},
		halfSpreadBPS: halfSpreadBPS,
		createdAt:     time.Now().UTC(),
		store:         store,
		wake:          make(chan struct{}, 1),
	}
	if store == nil {
		return c, nil
	}
	persisted, err := store.Load()
	if err != nil {
		return nil, fmt.Errorf("load control state: %w", err)
	}
	if saved := persisted.Control; saved != nil {
		c.killed, c.killReason, c.killedAt = saved.Killed, saved.KillReason, saved.KilledAt
		c.paused, c.pauseReason, c.pausedAt = saved.Paused, saved.PauseReason, saved.PausedAt
	}
	store.SetOverlay(c.overlay)
	return c, nil
}

// overlay runs under the store's lock. It takes c.mu, so no Controller method may call the store
// while holding c.mu.
func (c *Controller) overlay(p *state.Persistent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p.Control = &state.ControlState{
		Killed:      c.killed,
		KillReason:  c.killReason,
		KilledAt:    c.killedAt,
		Paused:      c.paused,
		PauseReason: c.pauseReason,
		PausedAt:    c.pausedAt,
	}
}

func (c *Controller) persist() error {
	if c.store == nil {
		return nil
	}
	return c.store.Update(nil)
}

func (c *Controller) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
		// A wake is already pending; the loop will read the latest state when it runs.
	}
}

// Wake fires after any change. main.go selects on it next to the poll ticker. Nil on a nil
// Controller, which blocks forever in a select -- i.e. never wakes.
func (c *Controller) Wake() <-chan struct{} {
	if c == nil {
		return nil
	}
	return c.wake
}

func (c *Controller) Status() Status {
	if c == nil {
		return Status{BidEnabled: true, AskEnabled: true, Adjust: Adjust{SizeMult: 1}}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return Status{
		Killed: c.killed, KillReason: c.killReason, KilledAt: c.killedAt,
		Paused: c.paused, PauseReason: c.pauseReason, PausedAt: c.pausedAt,
		BidEnabled: c.bidEnabled, AskEnabled: c.askEnabled, Adjust: c.adjust,
	}
}

// TakeDirty reports whether anything changed since the last call, and clears it.
func (c *Controller) TakeDirty() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	dirty := c.dirty
	c.dirty = false
	return dirty
}

// AllowPlace is checked immediately before every placement.
//
// The quoting cycle and a kill run concurrently: a cycle that loaded its snapshot before the kill
// arrived is still going to place its ladder, and the kill's cancel-all may already have listed the
// book. Checking here, after the kill flag is set and before each order is sent, closes all but the
// one request already in flight -- and the loop, woken by the kill, cancels that one next cycle.
func (c *Controller) AllowPlace(side exchange.Side) (bool, string) {
	if c == nil {
		return true, ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.killed:
		return false, HaltReasonControlKill
	case c.paused:
		return false, HaltReasonControlPause
	case side == exchange.SideBuy && !c.bidEnabled:
		return false, "control bid side disabled"
	case side == exchange.SideSell && !c.askEnabled:
		return false, "control ask side disabled"
	}
	return true, ""
}

// Kill is idempotent: a second kill keeps the original since, updates the reason if one is given,
// and still wakes the loop.
func (c *Controller) Kill(reason string) error {
	c.mu.Lock()
	if !c.killed {
		c.killed = true
		c.killedAt = time.Now().UTC()
	}
	if reason != "" || c.killReason == "" {
		c.killReason = reason
	}
	c.dirty = true
	c.mu.Unlock()
	c.signal()
	return c.persist()
}

func (c *Controller) Pause(reason string) error {
	c.mu.Lock()
	if !c.paused {
		c.paused = true
		c.pausedAt = time.Now().UTC()
	}
	if reason != "" || c.pauseReason == "" {
		c.pauseReason = reason
	}
	c.dirty = true
	c.mu.Unlock()
	c.signal()
	return c.persist()
}

// Resume clears a pause, and a kill only when clearKill is set. It never touches a risk halt: that
// is re-evaluated from market state on the next cycle, which is the only thing that can clear it.
func (c *Controller) Resume(clearKill bool) error {
	c.mu.Lock()
	if c.killed && !clearKill {
		c.mu.Unlock()
		return ErrKilled
	}
	if clearKill {
		c.killed, c.killReason, c.killedAt = false, "", time.Time{}
	}
	c.paused, c.pauseReason, c.pausedAt = false, "", time.Time{}
	c.dirty = true
	c.mu.Unlock()
	c.signal()
	return c.persist()
}

func (c *Controller) SetSide(side exchange.Side, enabled bool) error {
	c.mu.Lock()
	switch side {
	case exchange.SideBuy:
		c.bidEnabled = enabled
	case exchange.SideSell:
		c.askEnabled = enabled
	default:
		c.mu.Unlock()
		return fmt.Errorf("side must be bid or ask")
	}
	c.dirty = true
	c.mu.Unlock()
	c.signal()
	return nil
}

// SetAdjust validates every provided field before applying any, so a request with one bad field
// changes nothing.
func (c *Controller) SetAdjust(patch AdjustPatch) (Adjust, error) {
	if v := patch.MidShiftBPS; v != nil && !(math.Abs(*v) <= MaxMidShiftBPS) {
		return Adjust{}, fmt.Errorf("mid_shift_bps must be within [-%d, %d]", MaxMidShiftBPS, MaxMidShiftBPS)
	}
	if v := patch.SpreadAddBPS; v != nil {
		lower := -(c.halfSpreadBPS - 1)
		if !(*v >= lower && *v <= MaxSpreadAddBPS) {
			return Adjust{}, fmt.Errorf("spread_add_bps must be within [%s, %d]", formatBound(lower), MaxSpreadAddBPS)
		}
	}
	if v := patch.SizeMult; v != nil && !(*v >= 0 && *v <= MaxSizeMult) {
		return Adjust{}, fmt.Errorf("size_mult must be within [0, %d]", MaxSizeMult)
	}

	c.mu.Lock()
	if patch.MidShiftBPS != nil {
		c.adjust.MidShiftBPS = *patch.MidShiftBPS
	}
	if patch.SpreadAddBPS != nil {
		c.adjust.SpreadAddBPS = *patch.SpreadAddBPS
	}
	if patch.SizeMult != nil {
		c.adjust.SizeMult = *patch.SizeMult
	}
	adjust := c.adjust
	c.dirty = true
	c.mu.Unlock()
	c.signal()
	return adjust, nil
}

func formatBound(v float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.6f", v), "0"), ".")
}

// RecordAction appends to the ring of the last 50 place/cancel attempts.
func (c *Controller) RecordAction(a Action) {
	if c == nil {
		return
	}
	if a.At.IsZero() {
		a.At = time.Now().UTC()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.actions[c.actionsNext] = a
	c.actionsNext = (c.actionsNext + 1) % maxActions
	if c.actionsLen < maxActions {
		c.actionsLen++
	}
}

// Actions returns the recorded attempts oldest first.
func (c *Controller) Actions() []Action {
	out := make([]Action, 0, maxActions)
	if c == nil {
		return out
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	start := (c.actionsNext - c.actionsLen + maxActions) % maxActions
	for i := 0; i < c.actionsLen; i++ {
		out = append(out, c.actions[(start+i)%maxActions])
	}
	return out
}

// Resolve applies the state precedence KILLED > PAUSED > HALTED > RUNNING to the controller's
// overrides and the bot's last published view.
func (c *Controller) Resolve(view BotView, now time.Time) (State, string, string, time.Time) {
	st := c.Status()
	s, reason, source, since := resolveState(st, view)
	if since.IsZero() {
		if c != nil {
			since = c.createdAt
		} else {
			since = now
		}
	}
	return s, reason, source, since
}

func resolveState(st Status, view BotView) (State, string, string, time.Time) {
	switch {
	case st.Killed:
		return StateKilled, st.KillReason, SourceKillSwitch, st.KilledAt
	case view.KillSwitchFileActive:
		return StateKilled, HaltReasonKillSwitchFile, SourceKillSwitch, view.KillSwitchFileSince
	case st.Paused:
		return StatePaused, st.PauseReason, SourceManual, st.PausedAt
	case view.OperatorMode == "pause":
		return StatePaused, HaltReasonOperatorPause, SourceManual, view.HaltSince
	case view.Halted && !isOperatorHaltReason(view.HaltReason):
		return StateHalted, view.HaltReason, ClassifyHaltReason(view.HaltReason), view.HaltSince
	case !view.Initialized:
		return StateRunning, "starting up", SourceStartup, view.RunningSince
	}
	return StateRunning, "", "", view.RunningSince
}

func isOperatorHaltReason(reason string) bool {
	switch reason {
	case HaltReasonKillSwitchFile, HaltReasonControlKill, HaltReasonOperatorPause, HaltReasonControlPause:
		return true
	}
	return false
}

// ClassifyHaltReason maps a risk halt to reason_source. "stale" wins over everything, so "balances
// stale" is a feed problem; otherwise balance and RPC failures are the chain, and the rest -- no
// reference price, inventory and notional limits, the cancel budget -- are risk.
func ClassifyHaltReason(reason string) string {
	lower := strings.ToLower(reason)
	switch {
	case strings.Contains(lower, "stale"):
		return SourceStaleFeed
	case strings.Contains(lower, "balance"), strings.Contains(lower, "rpc"):
		return SourceChain
	}
	return SourceRisk
}
