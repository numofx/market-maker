package execution

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/control"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/metrics"
	"github.com/numofx/market-maker/internal/state"
	"github.com/numofx/market-maker/internal/strategy"
)

// cancelErrClient fails the cancels a test names, the way the venue does.
type cancelErrClient struct {
	mockClient
	errs map[string]error
}

func (c *cancelErrClient) CancelOrder(ctx context.Context, orderID string, reason string) error {
	if err := c.errs[orderID]; err != nil {
		return err
	}
	return c.mockClient.CancelOrder(ctx, orderID, reason)
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// A kill that stopped at the first failing order would leave the rest of the book resting, and on
// this venue a resting order stays executable on-chain until expiry. Not-found -- filled, already
// cancelled, or mid-settlement -- is off the book and reported as such, not as an error.
func TestCancelAllWithResultsReportsEachOrderAndOnlyTouchesTheRequestedSide(t *testing.T) {
	client := &cancelErrClient{
		mockClient: mockClient{openOrders: []exchange.Order{
			{ID: "bid-ok", Side: exchange.SideBuy},
			{ID: "bid-gone", Side: exchange.SideBuy},
			{ID: "bid-settling", Side: exchange.SideBuy},
			{ID: "bid-broken", Side: exchange.SideBuy},
			{ID: "ask-ok", Side: exchange.SideSell},
		}},
		errs: map[string]error{
			"bid-gone":     errors.New(`POST /v1/orders/cancel returned 404: {"error":"active order not found"}`),
			"bid-settling": fmt.Errorf("%w: lookup", exchange.ErrOrderNotFound),
			"bid-broken":   errors.New("POST /v1/orders/cancel returned 500: boom"),
		},
	}
	syncer := NewSyncer(client, exchange.MarketSpec{Symbol: "USDCcNGN-SPOT"}, config.Config{}, metrics.New(), quietLogger())

	results, err := syncer.CancelAllWithResults(context.Background(), "USDCcNGN-SPOT", cancelCategoryControlSide, exchange.SideBuy)
	if err != nil {
		t.Fatalf("CancelAllWithResults: %v", err)
	}
	got := map[string]string{}
	for _, r := range results {
		got[r.OrderID] = r.Result
	}
	want := map[string]string{
		"bid-ok":       control.CancelResultCancelled,
		"bid-gone":     control.CancelResultNotFound,
		"bid-settling": control.CancelResultNotFound,
		"bid-broken":   control.CancelResultError,
	}
	if len(got) != len(want) {
		t.Fatalf("results %+v, want exactly the four bids", results)
	}
	for id, result := range want {
		if got[id] != result {
			t.Fatalf("%s = %q, want %q (all results %+v)", id, got[id], result, results)
		}
	}
	for _, id := range client.cancelled {
		if id == "ask-ok" {
			t.Fatal("a bid-side pull cancelled an ask")
		}
	}
}

func killHarness(t *testing.T, store *state.Store) (*Bot, *integrationClient, *control.Controller) {
	t.Helper()
	client := &integrationClient{
		spec: exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "USDC", QuoteAsset: "cNGN", TickSize: 0.01, SizeStep: 0.1, MinSize: 0.1},
		book: exchange.Book{Bids: []exchange.BookLevel{{Price: 99}}, Asks: []exchange.BookLevel{{Price: 101}}},
		balances: []exchange.Balance{
			{Asset: "USDC", Total: 50, Available: 50},
			{Asset: "cNGN", Total: 10000, Available: 10000},
		},
		mockClient: mockClient{openOrders: []exchange.Order{
			{ID: "mm:USDCcNGN-SPOT:buy:10", Side: exchange.SideBuy, Price: 99.9, Size: 10, Managed: true, Nonce: "10"},
			{ID: "mm:USDCcNGN-SPOT:sell:11", Side: exchange.SideSell, Price: 100.1, Size: 10, Managed: true, Nonce: "11"},
		}},
	}
	cfg := config.Config{
		MarketSymbol: "USDCcNGN-SPOT", StateFile: filepath.Join(t.TempDir(), "state.json"),
		OrderSize: 10, HalfSpreadBPS: 10, MaxLongInventory: 100, MaxShortInventory: -100,
		CancelStaleOrderThreshold: 20, AdoptSizeTolerance: 0.01, OrderExpirySeconds: 60, ExpiryReplaceMarginSeconds: 15,
	}
	if store == nil {
		store = state.NewStore(cfg.StateFile)
	}
	ctrl, err := control.NewController(store, cfg.HalfSpreadBPS)
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}
	bot := NewBot(cfg, client, client.spec, metrics.New(), quietLogger(), store)
	bot.AttachController(ctrl)
	return bot, client, ctrl
}

func stateOf(bot *Bot, ctrl *control.Controller) control.State {
	s, _, _, _ := ctrl.Resolve(bot.View(), time.Now().UTC())
	return s
}

// The woken cycle after a kill: cancels everything it finds, places nothing, reads KILLED, and shows
// the book as the cancel left it rather than the pre-kill ladder. Resume with clear_kill quotes again.
func TestControlKillCancelsOnTheWokenCycleAndResumeQuotesAgain(t *testing.T) {
	bot, client, ctrl := killHarness(t, nil)
	if err := ctrl.Kill("drill"); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if err := bot.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle while killed: %v", err)
	}
	if len(client.cancelled) != 2 || len(client.placed) != 0 {
		t.Fatalf("cancelled %v placed %d, want both orders cancelled and nothing placed", client.cancelled, len(client.placed))
	}
	if s := stateOf(bot, ctrl); s != control.StateKilled {
		t.Fatalf("state %s, want KILLED", s)
	}
	if view := bot.View(); len(view.OpenOrders) != 0 || len(view.TargetBids) != 0 {
		t.Fatalf("view after kill shows orders %+v targets %+v; the cancel emptied the book", view.OpenOrders, view.TargetBids)
	}

	client.openOrders = nil
	client.cancelled = nil
	if err := ctrl.Resume(true); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if err := bot.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle after resume: %v", err)
	}
	if len(client.placed) != 2 {
		t.Fatalf("placed %d after resume, want both sides", len(client.placed))
	}
	if s := stateOf(bot, ctrl); s != control.StateRunning {
		t.Fatalf("state %s after resume, want RUNNING", s)
	}
}

// A cycle already past its kill check when the kill lands must not place its ladder behind the
// handler's cancel-all. The gate at place time is what stops it.
func TestAKillLandingMidCycleBlocksPlacement(t *testing.T) {
	client := &mockClient{}
	ctrl, _ := control.NewController(nil, 10)
	syncer := NewSyncer(client, exchange.MarketSpec{Symbol: "USDCcNGN-SPOT"}, config.Config{CancelStaleOrderThreshold: 10}, metrics.New(), quietLogger())
	syncer.control = ctrl
	if err := ctrl.Kill("mid-cycle"); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	result, err := syncer.Sync(context.Background(), state.Snapshot{Market: "USDCcNGN-SPOT"},
		strategy.Result{
			Bid: &strategy.Quote{Side: exchange.SideBuy, Price: 100, Size: 1},
			Ask: &strategy.Quote{Side: exchange.SideSell, Price: 101, Size: 1},
		},
		map[exchange.Side][]Identity{exchange.SideBuy: {{OrderID: "b", Nonce: "1"}}, exchange.SideSell: {{OrderID: "s", Nonce: "2"}}})
	if err != nil {
		t.Fatalf("a blocked placement must not fail the cycle: %v", err)
	}
	if len(client.placed) != 0 || result.Changed {
		t.Fatalf("placed %d changed %v while killed", len(client.placed), result.Changed)
	}
}

// A restart comes back KILLED. The kill is in the state file, and Initialize honours it before
// loading anything or reconciling -- a killed bot must not reconcile its way into adopting a ladder.
func TestPersistedKillHoldsAcrossARestart(t *testing.T) {
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	first, _, ctrl := killHarness(t, store)
	_ = first
	if err := ctrl.Kill("before restart"); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	bot, client, restored := killHarness(t, store)
	if !restored.Status().Killed {
		t.Fatal("controller did not restore the kill from the state file")
	}
	if err := bot.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if len(client.placed) != 0 || len(client.cancelled) != 2 {
		t.Fatalf("placed %d cancelled %v on a killed restart", len(client.placed), client.cancelled)
	}
	if bot.LastReconciliationResult().AdoptedBidOrderID != "" {
		t.Fatal("a killed restart adopted an order")
	}
	if s := stateOf(bot, restored); s != control.StateKilled {
		t.Fatalf("state %s, want KILLED", s)
	}
	// And the bot's own per-cycle save does not write the kill back out of the file.
	if err := bot.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	persisted, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if persisted.Control == nil || !persisted.Control.Killed {
		t.Fatalf("state file control = %+v after a cycle, want killed", persisted.Control)
	}
}

// lockedClient serializes the test double, so the race detector reports races in the bot rather than
// in the mock's unsynchronized slices.
type lockedClient struct {
	mu    sync.Mutex
	inner *integrationClient
}

func (l *lockedClient) GetBook(ctx context.Context, m string) (exchange.Book, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inner.GetBook(ctx, m)
}
func (l *lockedClient) GetTrades(ctx context.Context, m string) ([]exchange.Trade, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inner.GetTrades(ctx, m)
}
func (l *lockedClient) GetBalances(ctx context.Context) ([]exchange.Balance, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inner.GetBalances(ctx)
}
func (l *lockedClient) ListOpenOrders(ctx context.Context, m string) ([]exchange.Order, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	orders, err := l.inner.ListOpenOrders(ctx, m)
	return append([]exchange.Order(nil), orders...), err
}
func (l *lockedClient) PlaceLimitOrder(ctx context.Context, req exchange.PlaceOrderRequest) (exchange.Order, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inner.PlaceLimitOrder(ctx, req)
}
func (l *lockedClient) CancelOrder(ctx context.Context, id, reason string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inner.CancelOrder(ctx, id, reason)
}
func (l *lockedClient) CancelAllOrders(ctx context.Context, m, reason string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inner.CancelAllOrders(ctx, m, reason)
}
func (l *lockedClient) GetMarket(ctx context.Context, m string) (exchange.MarketSpec, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inner.GetMarket(ctx, m)
}
func (l *lockedClient) RequiredWorstFee(spec exchange.MarketSpec, price float64) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inner.RequiredWorstFee(spec, price)
}

// The handler's kill runs on an HTTP goroutine while the loop is mid-cycle. Run under -race, this is
// what backs the claim that CancelManaged, View and the controller are safe off the loop.
func TestKillFromAnotherGoroutineWhileCyclesRunIsRaceFree(t *testing.T) {
	_, inner, _ := killHarness(t, nil)
	client := &lockedClient{inner: inner}
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	ctrl, err := control.NewController(store, 10)
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}
	cfg := config.Config{
		MarketSymbol: "USDCcNGN-SPOT", StateFile: filepath.Join(t.TempDir(), "state.json"),
		OrderSize: 10, HalfSpreadBPS: 10, MaxLongInventory: 100, MaxShortInventory: -100,
		CancelStaleOrderThreshold: 20, AdoptSizeTolerance: 0.01, OrderExpirySeconds: 60, ExpiryReplaceMarginSeconds: 15,
	}
	bot := NewBot(cfg, client, inner.spec, metrics.New(), quietLogger(), store)
	bot.AttachController(ctrl)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			_ = bot.RunCycle(context.Background())
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if i == 5 {
				_ = ctrl.Kill("race")
			}
			_, _ = bot.CancelManaged(context.Background(), control.CancelScopeKill, "")
			_ = bot.View()
			_ = ctrl.Actions()
		}
	}()
	wg.Wait()

	if err := bot.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle after the race: %v", err)
	}
	if s := stateOf(bot, ctrl); s != control.StateKilled {
		t.Fatalf("state %s, want KILLED", s)
	}
}

// Dry run sends nothing. The kill response must say so rather than report a clean book, and the
// activity feed records what would have been sent, marked dry_run.
func TestDryRunCancelsAreReportedAsNotSent(t *testing.T) {
	client := &mockClient{openOrders: []exchange.Order{{ID: "o1", Side: exchange.SideBuy, Price: 1326, Size: 1}}}
	ctrl, _ := control.NewController(nil, 10)
	syncer := NewSyncer(client, exchange.MarketSpec{Symbol: "USDCcNGN-SPOT"}, config.Config{DryRun: true}, metrics.New(), quietLogger())
	syncer.control = ctrl

	results, err := syncer.CancelAllWithResults(context.Background(), "USDCcNGN-SPOT", cancelCategoryKillSwitch, "")
	if err != nil {
		t.Fatalf("CancelAllWithResults: %v", err)
	}
	if len(results) != 1 || results[0].Result != control.CancelResultError {
		t.Fatalf("results %+v, want one not-sent result", results)
	}
	if len(client.cancelled) != 0 {
		t.Fatalf("dry run sent cancels: %v", client.cancelled)
	}
	actions := ctrl.Actions()
	if len(actions) != 1 || !actions[0].DryRun || actions[0].Action != "cancel" || actions[0].OrderID != "o1" {
		t.Fatalf("actions %+v, want one dry-run cancel", actions)
	}
}
