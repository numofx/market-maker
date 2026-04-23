package execution

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/metrics"
	"github.com/numofx/market-maker/internal/state"
	"github.com/numofx/market-maker/internal/strategy"
)

type mockClient struct {
	openOrders []exchange.Order
	placed     []exchange.PlaceOrderRequest
	cancelled  []string
	placeErr   error
}

func (m *mockClient) GetBook(context.Context, string) (exchange.Book, error) {
	return exchange.Book{}, nil
}
func (m *mockClient) GetTrades(context.Context, string) ([]exchange.Trade, error) { return nil, nil }
func (m *mockClient) GetBalances(context.Context) ([]exchange.Balance, error)     { return nil, nil }
func (m *mockClient) CancelAllOrders(_ context.Context, _ string, _ string) error {
	for _, order := range m.openOrders {
		m.cancelled = append(m.cancelled, order.ID)
	}
	return nil
}
func (m *mockClient) GetMarket(context.Context, string) (exchange.MarketSpec, error) {
	return exchange.MarketSpec{}, nil
}
func (m *mockClient) ListOpenOrders(context.Context, string) ([]exchange.Order, error) {
	return m.openOrders, nil
}
func (m *mockClient) PlaceLimitOrder(_ context.Context, req exchange.PlaceOrderRequest) (exchange.Order, error) {
	m.placed = append(m.placed, req)
	return exchange.Order{ID: req.OrderID, Nonce: req.Nonce, Side: req.Side, Price: req.Price, Size: req.Size}, m.placeErr
}
func (m *mockClient) CancelOrder(_ context.Context, orderID string, _ string) error {
	m.cancelled = append(m.cancelled, orderID)
	return nil
}

func TestShouldCancel(t *testing.T) {
	tests := []struct {
		name    string
		current *exchange.Order
		target  *strategy.Quote
		want    bool
	}{
		{name: "missing target cancels", current: &exchange.Order{ID: "1", Side: exchange.SideBuy, Price: 100, Size: 1}, target: nil, want: true},
		{name: "large drift cancels", current: &exchange.Order{ID: "1", Side: exchange.SideBuy, Price: 100, Size: 1}, target: &strategy.Quote{Side: exchange.SideBuy, Price: 101, Size: 1}, want: true},
		{name: "same order kept", current: &exchange.Order{ID: "1", Side: exchange.SideBuy, Price: 100, Size: 1}, target: &strategy.Quote{Side: exchange.SideBuy, Price: 100.01, Size: 1}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldCancel(tt.current, tt.target, 20, nil)
			if got != tt.want {
				t.Fatalf("shouldCancel() = %v want %v", got, tt.want)
			}
		})
	}
}

func TestEvaluateCancelSuppression(t *testing.T) {
	now := time.Now().UTC()
	current := &exchange.Order{ID: "b1", Side: exchange.SideBuy, Price: 100, Size: 1, CreatedAt: now.Add(-2 * time.Second)}
	target := &strategy.Quote{Side: exchange.SideBuy, Price: 100.3, Size: 1}

	tests := []struct {
		name         string
		cfg          config.Config
		wantSuppress string
		wantCancel   bool
	}{
		{
			name: "replace suppressed by minimum lifetime",
			cfg: config.Config{
				CancelStaleOrderThreshold: 20,
				MinQuoteLifetime:          5 * time.Second,
			},
			wantSuppress: "minimum_lifetime_not_met",
		},
		{
			name: "replace suppressed by minimum move threshold",
			cfg: config.Config{
				CancelStaleOrderThreshold: 20,
				MinReplaceMoveBPS:         40,
			},
			wantSuppress: "minimum_move_not_met",
		},
		{
			name: "replace allowed when guards satisfied",
			cfg: config.Config{
				CancelStaleOrderThreshold: 20,
				MinQuoteLifetime:          time.Second,
				MinReplaceMoveBPS:         20,
			},
			wantCancel: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decision := evaluateCancel(current, target, nil, tt.cfg, time.Time{}, now)
			if decision.SuppressReason != tt.wantSuppress {
				t.Fatalf("suppress reason = %q want %q", decision.SuppressReason, tt.wantSuppress)
			}
			if decision.Cancel != tt.wantCancel {
				t.Fatalf("cancel = %v want %v", decision.Cancel, tt.wantCancel)
			}
		})
	}
}

func TestEvaluateCancelIgnoresDustSizeMismatch(t *testing.T) {
	now := time.Now().UTC()
	current := &exchange.Order{ID: "b1", Side: exchange.SideBuy, Price: 100, Size: 2.036266, CreatedAt: now.Add(-10 * time.Second)}
	target := &strategy.Quote{Side: exchange.SideBuy, Price: 100, Size: 2.036287}

	decision := evaluateCancel(current, target, nil, config.Config{
		CancelStaleOrderThreshold: 10,
		AdoptSizeTolerance:        0.000001,
	}, time.Time{}, now)
	if decision.Cancel {
		t.Fatalf("expected dust size mismatch to be kept, got cancel reason %q", decision.Reason)
	}
	if decision.Suppress {
		t.Fatalf("expected keep, not suppress, got %q", decision.SuppressReason)
	}
}

func TestEvaluateCancelReplacesMaterialSizeMismatch(t *testing.T) {
	now := time.Now().UTC()
	current := &exchange.Order{ID: "b1", Side: exchange.SideBuy, Price: 100, Size: 2.046505, CreatedAt: now.Add(-10 * time.Second)}
	target := &strategy.Quote{Side: exchange.SideBuy, Price: 100, Size: 2.036266}

	decision := evaluateCancel(current, target, nil, config.Config{
		CancelStaleOrderThreshold: 10,
		AdoptSizeTolerance:        0.000001,
	}, time.Time{}, now)
	if !decision.Cancel || decision.Reason != "size_mismatch" {
		t.Fatalf("expected material size mismatch replace, got cancel=%v reason=%q", decision.Cancel, decision.Reason)
	}
}

func TestSync(t *testing.T) {
	client := &mockClient{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	syncer := NewSyncer(client, exchange.MarketSpec{Symbol: "USDCcNGN-SPOT"}, config.Config{CancelStaleOrderThreshold: 10}, metrics.New(), logger)

	tests := []struct {
		name        string
		openOrders  []exchange.Order
		quotes      strategy.Result
		wantPlaces  int
		wantCancels int
	}{
		{
			name:       "places missing bid and ask",
			openOrders: nil,
			quotes: strategy.Result{
				Bid: &strategy.Quote{Side: exchange.SideBuy, Price: 100, Size: 1},
				Ask: &strategy.Quote{Side: exchange.SideSell, Price: 101, Size: 1},
			},
			wantPlaces:  2,
			wantCancels: 0,
		},
		{
			name: "cancels duplicate and stale order",
			openOrders: []exchange.Order{
				{ID: "b1", Side: exchange.SideBuy, Price: 100, Size: 1},
				{ID: "b2", Side: exchange.SideBuy, Price: 99, Size: 1},
			},
			quotes: strategy.Result{
				Bid: &strategy.Quote{Side: exchange.SideBuy, Price: 101, Size: 1},
			},
			wantPlaces:  1,
			wantCancels: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client.openOrders = tt.openOrders
			client.placed = nil
			client.cancelled = nil
			ids := map[exchange.Side]Identity{
				exchange.SideBuy:  {OrderID: "bid", Nonce: "10"},
				exchange.SideSell: {OrderID: "ask", Nonce: "11"},
			}
			_, err := syncer.Sync(context.Background(), state.Snapshot{Market: "USDCcNGN-SPOT", OpenOrders: tt.openOrders}, tt.quotes, ids)
			if err != nil {
				t.Fatalf("Sync() error = %v", err)
			}
			if len(client.placed) != tt.wantPlaces {
				t.Fatalf("placements = %d want %d", len(client.placed), tt.wantPlaces)
			}
			if len(client.cancelled) != tt.wantCancels {
				t.Fatalf("cancels = %d want %d", len(client.cancelled), tt.wantCancels)
			}
		})
	}
}

func TestSyncCancelRateLimit(t *testing.T) {
	client := &mockClient{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	syncer := NewSyncer(client, exchange.MarketSpec{Symbol: "USDCcNGN-SPOT"}, config.Config{
		CancelStaleOrderThreshold: 10,
		MaxCancelsPerMinute:       1,
	}, metrics.New(), logger)
	syncer.cancelTimestamps = []time.Time{time.Now().UTC().Add(-10 * time.Second)}

	_, err := syncer.Sync(context.Background(), state.Snapshot{
		Market: "USDCcNGN-SPOT",
		OpenOrders: []exchange.Order{
			{ID: "b1", Side: exchange.SideBuy, Price: 100, Size: 1, CreatedAt: time.Now().UTC().Add(-time.Minute)},
		},
	}, strategy.Result{
		Bid: &strategy.Quote{Side: exchange.SideBuy, Price: 101, Size: 1},
	}, map[exchange.Side]Identity{
		exchange.SideBuy: {OrderID: "bid", Nonce: "10"},
	})
	if err == nil {
		t.Fatal("expected cancel rate limit error")
	}
	if _, ok := err.(*CancelRateLimitError); !ok {
		t.Fatalf("expected CancelRateLimitError, got %T", err)
	}
}

func TestCancelMetricsByCategory(t *testing.T) {
	client := &mockClient{
		openOrders: []exchange.Order{
			{ID: "b1", Side: exchange.SideBuy, Price: 100, Size: 1, CreatedAt: time.Now().UTC().Add(-time.Minute)},
		},
	}
	reg := metrics.New()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	syncer := NewSyncer(client, exchange.MarketSpec{Symbol: "USDCcNGN-SPOT"}, config.Config{
		CancelStaleOrderThreshold: 10,
		MaxCancelsPerMinute:       10,
	}, reg, logger)

	_, err := syncer.Sync(context.Background(), state.Snapshot{
		Market:     "USDCcNGN-SPOT",
		OpenOrders: client.openOrders,
	}, strategy.Result{
		Bid: &strategy.Quote{Side: exchange.SideBuy, Price: 101, Size: 1},
	}, map[exchange.Side]Identity{
		exchange.SideBuy: {OrderID: "bid", Nonce: "10"},
	})
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	client.openOrders = []exchange.Order{{ID: "risk-bid", Side: exchange.SideBuy}, {ID: "risk-ask", Side: exchange.SideSell}}
	if err := syncer.CancelAll(context.Background(), "USDCcNGN-SPOT", cancelCategoryRiskTriggered); err != nil {
		t.Fatalf("CancelAll() error = %v", err)
	}

	rr := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	body := rr.Body.String()
	if !strings.Contains(body, `category="replace_driven"`) {
		t.Fatalf("metrics missing replace_driven category: %s", body)
	}
	if !strings.Contains(body, `category="risk_triggered"`) {
		t.Fatalf("metrics missing risk_triggered category: %s", body)
	}
}

func TestSyncSkipsProtectedOrderCancels(t *testing.T) {
	client := &mockClient{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	syncer := NewSyncer(client, exchange.MarketSpec{Symbol: "USDCcNGN-SPOT"}, config.Config{
		CancelStaleOrderThreshold: 10,
		ProtectedOrderIDPrefixes:  []string{"validation:"},
	}, metrics.New(), logger)

	_, err := syncer.Sync(context.Background(), state.Snapshot{
		Market: "USDCcNGN-SPOT",
		OpenOrders: []exchange.Order{
			{ID: "validation:cross-1", Side: exchange.SideBuy, Price: 100, Size: 1, CreatedAt: time.Now().UTC().Add(-time.Minute)},
		},
	}, strategy.Result{
		Bid: &strategy.Quote{Side: exchange.SideBuy, Price: 101, Size: 1},
	}, map[exchange.Side]Identity{
		exchange.SideBuy: {OrderID: "bid", Nonce: "10"},
	})
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(client.cancelled) != 0 {
		t.Fatalf("protected order should not be canceled, got %#v", client.cancelled)
	}
}
