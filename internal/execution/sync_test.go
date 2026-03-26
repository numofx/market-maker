package execution

import (
	"context"
	"io"
	"log/slog"
	"testing"

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
func (m *mockClient) CancelAllOrders(_ context.Context, _ string) error {
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
func (m *mockClient) CancelOrder(_ context.Context, orderID string) error {
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
