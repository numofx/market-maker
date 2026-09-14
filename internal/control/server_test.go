package control

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/state"
)

const testToken = "s3cret-token"

type cancelCall struct {
	scope CancelScope
	side  exchange.Side
}

// fakeBackend stands in for the bot. It hands back whatever results the test sets and records every
// cancel it was asked for, so a test can assert what the handler did rather than what it returned.
type fakeBackend struct {
	mu      sync.Mutex
	view    BotView
	results []CancelResult
	calls   []cancelCall
}

func (f *fakeBackend) View() BotView {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.view
}

func (f *fakeBackend) CancelManaged(_ context.Context, scope CancelScope, side exchange.Side) ([]CancelResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, cancelCall{scope: scope, side: side})
	out := f.results
	// The book is empty after the first cancel-all, which is what makes a second kill idempotent.
	f.results = nil
	return out, nil
}

func (f *fakeBackend) cancelCalls() []cancelCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]cancelCall(nil), f.calls...)
}

type harness struct {
	t       *testing.T
	store   *state.Store
	ctrl    *Controller
	backend *fakeBackend
	server  *Server
	handler http.Handler
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	ctrl, err := NewController(store, 25)
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}
	backend := &fakeBackend{view: BotView{Market: "USDCcNGN-SPOT", OperatorMode: "normal", Initialized: true}}
	server := NewServer("127.0.0.1:0", testToken, ctrl, backend, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return &harness{t: t, store: store, ctrl: ctrl, backend: backend, server: server, handler: server.Handler()}
}

func (h *harness) do(method, path, body string, authHeader string) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	return rr
}

func (h *harness) post(path, body string) *httptest.ResponseRecorder {
	return h.do(http.MethodPost, path, body, "Bearer "+testToken)
}

func decodeBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON (%d): %s", rr.Code, rr.Body.String())
	}
	return out
}

func TestEveryRequestNeedsTheBearerToken(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"missing", "", http.StatusUnauthorized},
		{"wrong token", "Bearer not-the-token", http.StatusUnauthorized},
		{"token without scheme", testToken, http.StatusUnauthorized},
		{"right token", "Bearer " + testToken, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := h.do(http.MethodGet, "/control/state", "", tc.header)
			if rr.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", rr.Code, tc.want, rr.Body.String())
			}
			if rr.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("content type %q; every response is JSON, 401s included", rr.Header().Get("Content-Type"))
			}
		})
	}
	// The kill endpoint most of all: an unauthenticated caller must not reach the cancel path.
	if rr := h.do(http.MethodPost, "/control/kill", `{}`, "Bearer wrong"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated kill returned %d", rr.Code)
	}
	if calls := h.backend.cancelCalls(); len(calls) != 0 {
		t.Fatalf("an unauthenticated request cancelled orders: %+v", calls)
	}
	if h.ctrl.Status().Killed {
		t.Fatal("an unauthenticated kill set the kill flag")
	}
}

func TestKillCancelsEverythingAndIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.backend.results = []CancelResult{
		{OrderID: "mm:USDCcNGN-SPOT:buy:1", Result: CancelResultCancelled},
		{OrderID: "mm:USDCcNGN-SPOT:sell:2", Result: CancelResultNotFound},
	}

	rr := h.post("/control/kill", `{"reason":"drill"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("kill status %d: %s", rr.Code, rr.Body.String())
	}
	body := decodeBody(t, rr)
	if body["ok"] != true || body["state"] != string(StateKilled) {
		t.Fatalf("kill response %v", body)
	}
	if results, _ := body["cancel_results"].([]any); len(results) != 2 {
		t.Fatalf("cancel_results %v, want both orders reported", body["cancel_results"])
	}
	select {
	case <-h.ctrl.Wake():
	default:
		t.Fatal("kill must wake the loop, not wait for the next poll")
	}

	// Again, with nothing left to cancel: still 200, still KILLED, still a (now empty) results array,
	// and still a cancel-all -- a second kill is a retry, and a retry must do the work again.
	rr = h.post("/control/kill", `{"reason":"drill again"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("second kill status %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"cancel_results":[]`) {
		t.Fatalf("second kill body %s; cancel_results must be present as an empty array", rr.Body.String())
	}
	calls := h.backend.cancelCalls()
	if len(calls) != 2 || calls[0] != (cancelCall{scope: CancelScopeKill}) || calls[1] != (cancelCall{scope: CancelScopeKill}) {
		t.Fatalf("cancel calls %+v, want two kill-scoped cancels of both sides", calls)
	}

	// A restart comes back KILLED: the kill is in the state file, not only in memory.
	restarted, err := NewController(h.store, 25)
	if err != nil {
		t.Fatalf("NewController after restart: %v", err)
	}
	if st := restarted.Status(); !st.Killed || st.KillReason != "drill again" {
		t.Fatalf("restarted status %+v, want killed", st)
	}
}

func TestResumeAfterKillRequiresClearKill(t *testing.T) {
	h := newHarness(t)
	h.post("/control/kill", `{"reason":"drill"}`)

	rr := h.post("/control/resume", `{"reason":"back"}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("resume without clear_kill: status %d, want 409", rr.Code)
	}
	if body := decodeBody(t, rr); body["ok"] != false || body["error"] != "killed; resume requires clear_kill=true" {
		t.Fatalf("resume without clear_kill body %v", body)
	}
	if !h.ctrl.Status().Killed {
		t.Fatal("a refused resume must leave the kill in place")
	}

	rr = h.post("/control/resume", `{"reason":"back","clear_kill":true}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("resume with clear_kill: status %d: %s", rr.Code, rr.Body.String())
	}
	if body := decodeBody(t, rr); body["state"] != string(StateRunning) {
		t.Fatalf("resume body %v, want RUNNING", body)
	}
}

// clear_kill cannot delete a file this process does not own, so it must not claim to have resumed.
func TestResumeIsRefusedWhileTheKillSwitchFileExists(t *testing.T) {
	h := newHarness(t)
	h.backend.view.KillSwitchFileActive = true

	if rr := h.post("/control/resume", `{"clear_kill":false}`); rr.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409", rr.Code)
	}
	rr := h.post("/control/resume", `{"clear_kill":true}`)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "MM_KILL_SWITCH_FILE") {
		t.Fatalf("status %d body %s, want 409 naming the file", rr.Code, rr.Body.String())
	}
}

func TestPauseCancelsAndResumeClearsIt(t *testing.T) {
	h := newHarness(t)
	rr := h.post("/control/pause", `{"reason":"news"}`)
	if rr.Code != http.StatusOK || decodeBody(t, rr)["state"] != string(StatePaused) {
		t.Fatalf("pause: %d %s", rr.Code, rr.Body.String())
	}
	if calls := h.backend.cancelCalls(); len(calls) != 1 || calls[0] != (cancelCall{scope: CancelScopePause}) {
		t.Fatalf("pause cancel calls %+v", calls)
	}
	if rr := h.post("/control/resume", `{}`); rr.Code != http.StatusOK || decodeBody(t, rr)["state"] != string(StateRunning) {
		t.Fatalf("resume after pause: %d %s", rr.Code, rr.Body.String())
	}
}

func TestSidePullCancelsOnlyThatSide(t *testing.T) {
	h := newHarness(t)

	if rr := h.post("/control/side", `{"side":"bid","enabled":false,"reason":"one-way flow"}`); rr.Code != http.StatusOK {
		t.Fatalf("pull bid: %d %s", rr.Code, rr.Body.String())
	}
	calls := h.backend.cancelCalls()
	if len(calls) != 1 || calls[0] != (cancelCall{scope: CancelScopeSide, side: exchange.SideBuy}) {
		t.Fatalf("cancel calls %+v, want one bid-side cancel", calls)
	}
	if st := h.ctrl.Status(); st.BidEnabled || !st.AskEnabled {
		t.Fatalf("sides after pull: %+v", st)
	}
	if ok, _ := h.ctrl.AllowPlace(exchange.SideBuy); ok {
		t.Fatal("a pulled bid must be refused at place time")
	}
	if ok, _ := h.ctrl.AllowPlace(exchange.SideSell); !ok {
		t.Fatal("the ask was not pulled and must still place")
	}

	// Restoring a side cancels nothing.
	if rr := h.post("/control/side", `{"side":"bid","enabled":true}`); rr.Code != http.StatusOK {
		t.Fatalf("restore bid: %d", rr.Code)
	}
	if calls := h.backend.cancelCalls(); len(calls) != 1 {
		t.Fatalf("restoring a side cancelled orders: %+v", calls)
	}

	for _, body := range []string{`{"side":"buy","enabled":false}`, `{"side":"ask"}`} {
		if rr := h.post("/control/side", body); rr.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400", body, rr.Code)
		}
	}
}

func TestAdjustBoundsNameTheField(t *testing.T) {
	h := newHarness(t) // half spread 25 bps, so spread_add_bps may go down to -24
	cases := []struct {
		body  string
		field string
	}{
		{`{"mid_shift_bps":200.5}`, "mid_shift_bps"},
		{`{"mid_shift_bps":-201}`, "mid_shift_bps"},
		{`{"spread_add_bps":-25}`, "spread_add_bps"},
		{`{"spread_add_bps":501}`, "spread_add_bps"},
		{`{"size_mult":-0.1}`, "size_mult"},
		{`{"size_mult":5.01}`, "size_mult"},
		// One bad field rejects the whole request, including the good one before it.
		{`{"mid_shift_bps":10,"size_mult":6}`, "size_mult"},
	}
	for _, tc := range cases {
		rr := h.post("/control/adjust", tc.body)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400", tc.body, rr.Code)
		}
		if msg, _ := decodeBody(t, rr)["error"].(string); !strings.Contains(msg, tc.field) {
			t.Fatalf("%s: error %q does not name %s", tc.body, msg, tc.field)
		}
	}
	if adj := h.ctrl.Status().Adjust; adj != (Adjust{SizeMult: 1}) {
		t.Fatalf("rejected adjusts changed state: %+v", adj)
	}

	for _, body := range []string{`{"mid_shift_bps":-200}`, `{"spread_add_bps":-24}`, `{"size_mult":0}`} {
		if rr := h.post("/control/adjust", body); rr.Code != http.StatusOK {
			t.Fatalf("%s at the bound: status %d %s", body, rr.Code, rr.Body.String())
		}
	}
	// Partial: a later request that only sets size_mult leaves the shift and spread where they were.
	rr := h.post("/control/adjust", `{"size_mult":2,"reason":"deeper"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("partial adjust: %d", rr.Code)
	}
	if adj := h.ctrl.Status().Adjust; adj != (Adjust{MidShiftBPS: -200, SpreadAddBPS: -24, SizeMult: 2}) {
		t.Fatalf("adjust after partial update: %+v", adj)
	}
}

func TestStatePrecedence(t *testing.T) {
	killed := Status{Killed: true, KillReason: "k", Paused: true, PauseReason: "p", BidEnabled: true, AskEnabled: true}
	paused := Status{Paused: true, PauseReason: "p", BidEnabled: true, AskEnabled: true}
	none := Status{BidEnabled: true, AskEnabled: true}
	riskHalt := BotView{Initialized: true, Halted: true, HaltReason: "reference price unavailable"}

	cases := []struct {
		name       string
		status     Status
		view       BotView
		wantState  State
		wantSource string
	}{
		{"kill beats pause and risk", killed, riskHalt, StateKilled, SourceKillSwitch},
		{"kill switch file is a kill", paused, BotView{Initialized: true, KillSwitchFileActive: true, Halted: true, HaltReason: "x"}, StateKilled, SourceKillSwitch},
		{"pause beats risk", paused, riskHalt, StatePaused, SourceManual},
		{"operator mode pause is a pause", none, BotView{Initialized: true, OperatorMode: "pause", Halted: true, HaltReason: HaltReasonOperatorPause}, StatePaused, SourceManual},
		{"risk halt", none, riskHalt, StateHalted, SourceRisk},
		{"stale feed", none, BotView{Initialized: true, Halted: true, HaltReason: "balances stale"}, StateHalted, SourceStaleFeed},
		{"balance failure is chain", none, BotView{Initialized: true, Halted: true, HaltReason: "available base balance below threshold for USDC"}, StateHalted, SourceChain},
		// Just resumed: the bot still carries the pause as its halt until the woken cycle clears it.
		// That is not a risk halt and must not flash HALTED at the operator.
		{"leftover operator halt is not HALTED", none, BotView{Initialized: true, Halted: true, HaltReason: HaltReasonControlPause}, StateRunning, ""},
		{"starting up", none, BotView{}, StateRunning, SourceStartup},
		{"running", none, BotView{Initialized: true}, StateRunning, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotState, _, gotSource, _ := resolveState(tc.status, tc.view)
			if gotState != tc.wantState || gotSource != tc.wantSource {
				t.Fatalf("got %s/%q, want %s/%q", gotState, gotSource, tc.wantState, tc.wantSource)
			}
		})
	}
}

// With no token there is no listener at all -- not a listener that rejects everything.
func TestListenerDoesNotStartWithoutAToken(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctrl, _ := NewController(nil, 25)

	disabled := NewServer("127.0.0.1:0", "", ctrl, &fakeBackend{}, logger)
	started, err := disabled.Start()
	if err != nil || started || disabled.Addr() != nil {
		t.Fatalf("started=%v addr=%v err=%v, want no listener", started, disabled.Addr(), err)
	}

	enabled := NewServer("127.0.0.1:0", testToken, ctrl, &fakeBackend{}, logger)
	started, err = enabled.Start()
	if err != nil || !started || enabled.Addr() == nil {
		t.Fatalf("started=%v addr=%v err=%v, want a listener", started, enabled.Addr(), err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = enabled.Shutdown(ctx)
	}()
	resp, err := http.Get("http://" + enabled.Addr().String() + "/control/state")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET over the wire: %d, want 401", resp.StatusCode)
	}
}

// The terminal is built against these keys. Collections must be arrays and objects, never null.
func TestStateResponseShape(t *testing.T) {
	h := newHarness(t)
	h.ctrl.RecordAction(Action{Action: "place", Side: "buy", Price: 1370, Size: 5, OrderID: "o1", Reason: "quote"})
	rr := h.do(http.MethodGet, "/control/state", "", "Bearer "+testToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("state: %d", rr.Code)
	}
	body := decodeBody(t, rr)
	for _, key := range []string{
		"server_time", "market", "state", "reason", "reason_source", "since", "operator_mode", "dry_run",
		"sides", "adjust", "last_cycle_at", "last_cycle_error", "reference_price", "reference_source",
		"best_bid", "best_ask", "target_quotes", "open_orders", "positions", "config", "halt_count",
		"last_halt_reason", "last_actions",
	} {
		if _, ok := body[key]; !ok {
			t.Fatalf("state response missing %q: %s", key, rr.Body.String())
		}
	}
	if _, ok := body["open_orders"].([]any); !ok {
		t.Fatalf("open_orders = %v, want []", body["open_orders"])
	}
	if _, ok := body["positions"].(map[string]any); !ok {
		t.Fatalf("positions = %v, want {}", body["positions"])
	}
	targets := body["target_quotes"].(map[string]any)
	if _, ok := targets["bids"].([]any); !ok {
		t.Fatalf("target_quotes.bids = %v, want []", targets["bids"])
	}
	if sides := body["sides"].(map[string]any); sides["bid"].(map[string]any)["enabled"] != true {
		t.Fatalf("sides = %v", sides)
	}
	if actions := body["last_actions"].([]any); len(actions) != 1 {
		t.Fatalf("last_actions = %v", actions)
	}
	if _, err := time.Parse(time.RFC3339Nano, body["since"].(string)); err != nil {
		t.Fatalf("since %v is not RFC3339Nano: %v", body["since"], err)
	}
}

func TestActionsKeepTheLastFiftyOldestFirst(t *testing.T) {
	ctrl, _ := NewController(nil, 25)
	for i := 0; i < 60; i++ {
		ctrl.RecordAction(Action{Action: "cancel", OrderID: "o" + string(rune('A'+i))})
	}
	actions := ctrl.Actions()
	if len(actions) != 50 {
		t.Fatalf("len %d, want 50", len(actions))
	}
	if actions[0].OrderID != "o"+string(rune('A'+10)) || actions[49].OrderID != "o"+string(rune('A'+59)) {
		t.Fatalf("ring order wrong: first %s last %s", actions[0].OrderID, actions[49].OrderID)
	}
}
