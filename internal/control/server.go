package control

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/numofx/market-maker/internal/exchange"
)

// cancelDeadline bounds the cancel-everything a kill, pause or side pull runs inside the request.
// Detached from the request context on purpose: a terminal that disconnects mid-kill must not abort
// the cancels half-way through the book.
const cancelDeadline = 10 * time.Second

type CancelScope int

const (
	CancelScopeKill CancelScope = iota
	CancelScopePause
	CancelScopeSide
)

const (
	CancelResultCancelled = "cancelled"
	CancelResultNotFound  = "not_found"
	CancelResultError     = "error"
)

type CancelResult struct {
	OrderID string `json:"order_id"`
	Result  string `json:"result"`
	Error   string `json:"error"`
}

// Backend is what the control API needs from the bot. Both methods are called from HTTP goroutines
// while the quoting loop runs.
type Backend interface {
	// View returns the bot's last published state, plus a live read of the kill-switch file.
	View() BotView
	// CancelManaged cancels the bot's managed orders now; side "" means both sides.
	CancelManaged(ctx context.Context, scope CancelScope, side exchange.Side) ([]CancelResult, error)
}

// BotView is published by the bot at the end of every cycle.
//
// Units are the bot's own, which for USDCcNGN-SPOT are UI terms throughout: exchange.Order and
// strategy.Quote are converted by spotUIFromEngine on the way in and spotEngineFromUI on the way out,
// so a price is cNGN per USDC, a size is USDC, and side "buy" is a bid for USDC (the engine's sell of
// cNGN). Nothing here is engine-oriented.
type BotView struct {
	Market               string
	OperatorMode         string
	DryRun               bool
	Initialized          bool
	KillSwitchFileActive bool
	KillSwitchFileSince  time.Time
	Halted               bool
	HaltReason           string
	HaltSince            time.Time
	RunningSince         time.Time
	LastCycleAt          time.Time
	LastCycleError       string
	ReferencePrice       float64
	ReferenceSource      string
	BestBid              float64
	BestAsk              float64
	TargetBids           []Level
	TargetAsks           []Level
	OpenOrders           []OpenOrder
	Positions            map[string]Position
	Config               ConfigView
	HaltCount            uint64
	LastHaltReason       string
}

// Level is one target quote: price in cNGN per USDC, size in USDC.
type Level struct {
	Price float64 `json:"price"`
	Size  float64 `json:"size"`
}

// OpenOrder is a resting order as of the last cycle's load (or the last halt's cancel-all).
// Side is UI side (buy = bid for USDC), price cNGN per USDC, size remaining USDC, expiry unix seconds.
type OpenOrder struct {
	OrderID   string    `json:"order_id"`
	Side      string    `json:"side"`
	Price     float64   `json:"price"`
	Size      float64   `json:"size"`
	Nonce     string    `json:"nonce"`
	CreatedAt time.Time `json:"created_at"`
	Expiry    int64     `json:"expiry"`
	PostOnly  bool      `json:"post_only"`
}

// Position is keyed by asset symbol ("USDC", "cNGN") in human units.
type Position struct {
	Total     float64 `json:"total"`
	Reserved  float64 `json:"reserved"`
	Available float64 `json:"available"`
}

type ConfigView struct {
	HalfSpreadBPS              float64 `json:"half_spread_bps"`
	QuoteLevels                int     `json:"quote_levels"`
	OrderSize                  float64 `json:"order_size"`
	OrderExpirySeconds         int64   `json:"order_expiry_seconds"`
	ExpiryReplaceMarginSeconds int64   `json:"expiry_replace_margin_seconds"`
	PostOnly                   bool    `json:"post_only"`
	MaxNetInventory            float64 `json:"max_net_inventory"`
	MaxNotionalPerSide         float64 `json:"max_notional_per_side"`
}

type SideView struct {
	Enabled bool `json:"enabled"`
}

type Sides struct {
	Bid SideView `json:"bid"`
	Ask SideView `json:"ask"`
}

type TargetQuotes struct {
	Bids []Level `json:"bids"`
	Asks []Level `json:"asks"`
}

// StateResponse is GET /control/state. Times are RFC3339Nano; a time that has never happened (no
// cycle has run yet) is Go's zero time, 0001-01-01T00:00:00Z, rather than null.
type StateResponse struct {
	ServerTime      time.Time           `json:"server_time"`
	Market          string              `json:"market"`
	State           State               `json:"state"`
	Reason          string              `json:"reason"`
	ReasonSource    string              `json:"reason_source"`
	Since           time.Time           `json:"since"`
	OperatorMode    string              `json:"operator_mode"`
	DryRun          bool                `json:"dry_run"`
	Sides           Sides               `json:"sides"`
	Adjust          Adjust              `json:"adjust"`
	LastCycleAt     time.Time           `json:"last_cycle_at"`
	LastCycleError  string              `json:"last_cycle_error"`
	ReferencePrice  float64             `json:"reference_price"`
	ReferenceSource string              `json:"reference_source"`
	BestBid         float64             `json:"best_bid"`
	BestAsk         float64             `json:"best_ask"`
	TargetQuotes    TargetQuotes        `json:"target_quotes"`
	OpenOrders      []OpenOrder         `json:"open_orders"`
	Positions       map[string]Position `json:"positions"`
	Config          ConfigView          `json:"config"`
	HaltCount       uint64              `json:"halt_count"`
	LastHaltReason  string              `json:"last_halt_reason"`
	LastActions     []Action            `json:"last_actions"`
}

type killResponse struct {
	OK            bool           `json:"ok"`
	State         State          `json:"state"`
	CancelResults []CancelResult `json:"cancel_results"`
	CancelError   string         `json:"cancel_error,omitempty"`
	PersistError  string         `json:"persist_error,omitempty"`
}

type actionResponse struct {
	OK            bool           `json:"ok"`
	State         State          `json:"state"`
	CancelResults []CancelResult `json:"cancel_results,omitempty"`
	CancelError   string         `json:"cancel_error,omitempty"`
	PersistError  string         `json:"persist_error,omitempty"`
	Adjust        *Adjust        `json:"adjust,omitempty"`
}

type errorResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// Server is the control API listener, separate from the metrics server so that it can bind to
// loopback while /metrics and /healthz stay reachable to the platform.
type Server struct {
	addr    string
	token   string
	ctrl    *Controller
	backend Backend
	logger  *slog.Logger
	srv     *http.Server
	ln      net.Listener
}

func NewServer(addr, token string, ctrl *Controller, backend Backend, logger *slog.Logger) *Server {
	return &Server{addr: addr, token: token, ctrl: ctrl, backend: backend, logger: logger}
}

// Start listens only when a token is configured. An unauthenticated kill endpoint is worse than none:
// anything that can reach the port could pull the book -- or, via resume, put it back.
//
// A bind failure with a token set is returned, and main exits on it: the operator configured a kill
// path, and running without the one they expect is not a degraded mode to limp along in.
func (s *Server) Start() (bool, error) {
	if s.token == "" {
		s.logger.Info("control API disabled", "reason", "MM_CONTROL_TOKEN is not set")
		return false, nil
	}
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return false, fmt.Errorf("control API listen %s: %w", s.addr, err)
	}
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok && !tcp.IP.IsLoopback() {
		s.logger.Warn("control API is listening on a non-loopback address", "addr", ln.Addr().String(),
			"note", "the terminal sidecar shares the task network namespace and only needs 127.0.0.1")
	}
	s.ln = ln
	s.srv = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// Longer than cancelDeadline, or a slow kill would have its response cut off after the
		// cancels ran, and the terminal would retry a kill that already happened.
		WriteTimeout: cancelDeadline + 20*time.Second,
	}
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error("control API server failed", "error", err)
		}
	}()
	s.logger.Info("control API listening", "addr", ln.Addr().String())
	return true, nil
}

// Addr is the bound address, or nil when the listener was never started.
func (s *Server) Addr() net.Addr {
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

func (s *Server) Shutdown(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !s.authorized(r) {
			s.logger.Warn("control_unauthorized", "path", r.URL.Path, "remote_addr", r.RemoteAddr)
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
			return
		}
		route := map[string]struct {
			method string
			fn     func(http.ResponseWriter, *http.Request)
		}{
			"/control/state":  {http.MethodGet, s.handleState},
			"/control/kill":   {http.MethodPost, s.handleKill},
			"/control/pause":  {http.MethodPost, s.handlePause},
			"/control/resume": {http.MethodPost, s.handleResume},
			"/control/side":   {http.MethodPost, s.handleSide},
			"/control/adjust": {http.MethodPost, s.handleAdjust},
		}[r.URL.Path]
		if route.fn == nil {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "not found"})
			return
		}
		if r.Method != route.method {
			w.Header().Set("Allow", route.method)
			writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "method not allowed; use " + route.method})
			return
		}
		route.fn(w, r)
	})
}

// authorized compares SHA-256 digests in constant time, so neither the token's contents nor its
// length leak through response timing. An empty configured token authorizes nothing.
func (s *Server) authorized(r *http.Request) bool {
	if s.token == "" {
		return false
	}
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	got := sha256.Sum256([]byte(header[len(prefix):]))
	want := sha256.Sum256([]byte(s.token))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

func (s *Server) handleState(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.buildState(time.Now().UTC()))
}

func (s *Server) buildState(now time.Time) StateResponse {
	view := s.backend.View()
	st := s.ctrl.Status()
	state, reason, source, since := s.ctrl.Resolve(view, now)
	resp := StateResponse{
		ServerTime:      now,
		Market:          view.Market,
		State:           state,
		Reason:          reason,
		ReasonSource:    source,
		Since:           since,
		OperatorMode:    view.OperatorMode,
		DryRun:          view.DryRun,
		Sides:           Sides{Bid: SideView{Enabled: st.BidEnabled}, Ask: SideView{Enabled: st.AskEnabled}},
		Adjust:          st.Adjust,
		LastCycleAt:     view.LastCycleAt,
		LastCycleError:  view.LastCycleError,
		ReferencePrice:  view.ReferencePrice,
		ReferenceSource: view.ReferenceSource,
		BestBid:         view.BestBid,
		BestAsk:         view.BestAsk,
		TargetQuotes:    TargetQuotes{Bids: nonNilLevels(view.TargetBids), Asks: nonNilLevels(view.TargetAsks)},
		OpenOrders:      view.OpenOrders,
		Positions:       view.Positions,
		Config:          view.Config,
		HaltCount:       view.HaltCount,
		LastHaltReason:  view.LastHaltReason,
		LastActions:     s.ctrl.Actions(),
	}
	if resp.OpenOrders == nil {
		resp.OpenOrders = []OpenOrder{}
	}
	if resp.Positions == nil {
		resp.Positions = map[string]Position{}
	}
	return resp
}

func (s *Server) currentState() State {
	state, _, _, _ := s.ctrl.Resolve(s.backend.View(), time.Now().UTC())
	return state
}

func (s *Server) handleKill(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason string `json:"reason"`
	}
	if !s.decode(w, r, "kill", &body) {
		return
	}
	resp := killResponse{OK: true}
	// Flag first, then cancel: once the flag is set, AllowPlace stops a concurrently running cycle
	// from placing anything behind the cancel-all.
	if err := s.ctrl.Kill(body.Reason); err != nil {
		resp.PersistError = err.Error()
		s.logger.Error("control_persist_failed", "action", "kill", "error", err)
	}
	resp.CancelResults, resp.CancelError = s.cancel(CancelScopeKill, "")
	resp.State = s.currentState()
	s.logAction(r, "kill", body.Reason, resp.State, resp.CancelResults, resp.CancelError)
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason string `json:"reason"`
	}
	if !s.decode(w, r, "pause", &body) {
		return
	}
	resp := actionResponse{OK: true}
	if err := s.ctrl.Pause(body.Reason); err != nil {
		resp.PersistError = err.Error()
		s.logger.Error("control_persist_failed", "action", "pause", "error", err)
	}
	resp.CancelResults, resp.CancelError = s.cancel(CancelScopePause, "")
	resp.State = s.currentState()
	s.logAction(r, "pause", body.Reason, resp.State, resp.CancelResults, resp.CancelError)
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason    string `json:"reason"`
		ClearKill bool   `json:"clear_kill"`
	}
	if !s.decode(w, r, "resume", &body) {
		return
	}
	view := s.backend.View()
	st := s.ctrl.Status()
	if (st.Killed || view.KillSwitchFileActive) && !body.ClearKill {
		s.reject(w, r, "resume", body.Reason, http.StatusConflict, ErrKilled.Error())
		return
	}
	// clear_kill cannot remove a file this process does not own. Refused before changing anything,
	// so the response never claims a resume that the next cycle would immediately undo.
	if view.KillSwitchFileActive {
		s.reject(w, r, "resume", body.Reason, http.StatusConflict, "kill switch file present; remove MM_KILL_SWITCH_FILE to resume")
		return
	}
	resp := actionResponse{OK: true}
	if err := s.ctrl.Resume(body.ClearKill); err != nil {
		if errors.Is(err, ErrKilled) {
			s.reject(w, r, "resume", body.Reason, http.StatusConflict, err.Error())
			return
		}
		resp.PersistError = err.Error()
		s.logger.Error("control_persist_failed", "action", "resume", "error", err)
	}
	resp.State = s.currentState()
	s.logger.Info("control_action", "action", "resume", "reason", body.Reason, "clear_kill", body.ClearKill,
		"state", resp.State, "remote_addr", r.RemoteAddr)
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleSide(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Side    string `json:"side"`
		Enabled *bool  `json:"enabled"`
		Reason  string `json:"reason"`
	}
	if !s.decode(w, r, "side", &body) {
		return
	}
	var side exchange.Side
	switch body.Side {
	case "bid":
		side = exchange.SideBuy
	case "ask":
		side = exchange.SideSell
	default:
		s.reject(w, r, "side", body.Reason, http.StatusBadRequest, "side must be bid or ask")
		return
	}
	if body.Enabled == nil {
		s.reject(w, r, "side", body.Reason, http.StatusBadRequest, "enabled is required")
		return
	}
	if err := s.ctrl.SetSide(side, *body.Enabled); err != nil {
		s.reject(w, r, "side", body.Reason, http.StatusBadRequest, err.Error())
		return
	}
	resp := actionResponse{OK: true}
	if !*body.Enabled {
		resp.CancelResults, resp.CancelError = s.cancel(CancelScopeSide, side)
	}
	resp.State = s.currentState()
	s.logger.Info("control_action", "action", "side", "side", body.Side, "enabled", *body.Enabled,
		"reason", body.Reason, "state", resp.State, "remote_addr", r.RemoteAddr,
		"cancel_results", summarizeCancels(resp.CancelResults), "cancel_error", resp.CancelError)
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleAdjust(w http.ResponseWriter, r *http.Request) {
	var patch AdjustPatch
	if !s.decode(w, r, "adjust", &patch) {
		return
	}
	adjust, err := s.ctrl.SetAdjust(patch)
	if err != nil {
		s.reject(w, r, "adjust", patch.Reason, http.StatusBadRequest, err.Error())
		return
	}
	resp := actionResponse{OK: true, State: s.currentState(), Adjust: &adjust}
	s.logger.Info("control_action", "action", "adjust", "reason", patch.Reason, "state", resp.State,
		"mid_shift_bps", adjust.MidShiftBPS, "spread_add_bps", adjust.SpreadAddBPS, "size_mult", adjust.SizeMult,
		"remote_addr", r.RemoteAddr)
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) cancel(scope CancelScope, side exchange.Side) ([]CancelResult, string) {
	ctx, cancel := context.WithTimeout(context.Background(), cancelDeadline)
	defer cancel()
	results, err := s.backend.CancelManaged(ctx, scope, side)
	if results == nil {
		results = []CancelResult{}
	}
	if err != nil {
		return results, err.Error()
	}
	return results, ""
}

// decode accepts an empty body as "no fields", so `curl -X POST .../kill` with no payload works.
func (s *Server) decode(w http.ResponseWriter, r *http.Request, action string, into any) bool {
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(into)
	if err == nil || errors.Is(err, io.EOF) {
		return true
	}
	s.reject(w, r, action, "", http.StatusBadRequest, "invalid JSON body: "+err.Error())
	return false
}

func (s *Server) reject(w http.ResponseWriter, r *http.Request, action, reason string, status int, message string) {
	s.logger.Warn("control_action", "action", action, "reason", reason, "rejected", message,
		"status", status, "state", s.currentState(), "remote_addr", r.RemoteAddr)
	writeJSON(w, status, errorResponse{Error: message})
}

func (s *Server) logAction(r *http.Request, action, reason string, state State, results []CancelResult, cancelErr string) {
	s.logger.Info("control_action", "action", action, "reason", reason, "state", state,
		"remote_addr", r.RemoteAddr, "cancel_results", summarizeCancels(results), "cancel_error", cancelErr)
}

func summarizeCancels(results []CancelResult) map[string]int {
	counts := map[string]int{}
	for _, result := range results {
		counts[result.Result]++
	}
	return counts
}

func nonNilLevels(levels []Level) []Level {
	if levels == nil {
		return []Level{}
	}
	return levels
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
