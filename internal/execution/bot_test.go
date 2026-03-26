package execution

import (
	"testing"
	"time"

	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/state"
)

func TestObserveOrderStateFills(t *testing.T) {
	tests := []struct {
		name         string
		previous     state.Snapshot
		current      state.Snapshot
		wantSide     string
		wantCount    uint64
		wantPartials uint64
		wantTruth    bool
	}{
		{
			name: "full fill from order disappearance",
			previous: state.Snapshot{OpenOrders: []exchange.Order{
				{ID: "bid-1", Side: exchange.SideBuy, Size: 10},
			}},
			current:      state.Snapshot{},
			wantSide:     string(exchange.SideBuy),
			wantCount:    1,
			wantPartials: 0,
			wantTruth:    true,
		},
		{
			name: "partial fill from remaining size reduction",
			previous: state.Snapshot{OpenOrders: []exchange.Order{
				{ID: "ask-1", Side: exchange.SideSell, Size: 10},
			}},
			current: state.Snapshot{OpenOrders: []exchange.Order{
				{ID: "ask-1", Side: exchange.SideSell, Size: 6},
			}},
			wantSide:     string(exchange.SideSell),
			wantCount:    1,
			wantPartials: 1,
			wantTruth:    true,
		},
		{
			name: "no fill when unchanged",
			previous: state.Snapshot{OpenOrders: []exchange.Order{
				{ID: "bid-1", Side: exchange.SideBuy, Size: 10},
			}},
			current: state.Snapshot{OpenOrders: []exchange.Order{
				{ID: "bid-1", Side: exchange.SideBuy, Size: 10},
			}},
			wantTruth: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, partials, truth := observeOrderStateFills(tt.previous, tt.current)
			if truth != tt.wantTruth {
				t.Fatalf("truth = %v want %v", truth, tt.wantTruth)
			}
			if tt.wantTruth {
				if got[tt.wantSide] != tt.wantCount {
					t.Fatalf("fills[%s] = %d want %d", tt.wantSide, got[tt.wantSide], tt.wantCount)
				}
				if partials != tt.wantPartials {
					t.Fatalf("partials = %d want %d", partials, tt.wantPartials)
				}
			}
		})
	}
}

func TestObserveTradeFills(t *testing.T) {
	previous := state.Snapshot{RecentTrades: []exchange.Trade{{ID: 1, Side: exchange.SideBuy}}}
	current := state.Snapshot{RecentTrades: []exchange.Trade{
		{ID: 1, Side: exchange.SideBuy},
		{ID: 2, Side: exchange.SideBuy},
		{ID: 3, Side: exchange.SideSell},
	}}
	got := observeTradeFills(previous, current)
	if got[string(exchange.SideSell)] != 1 {
		t.Fatalf("sell fills = %d want 1", got[string(exchange.SideSell)])
	}
	if got[string(exchange.SideBuy)] != 1 {
		t.Fatalf("buy fills = %d want 1", got[string(exchange.SideBuy)])
	}
}

func TestExchangeObservedQuoteAgeFallback(t *testing.T) {
	if age := exchangeObservedQuoteAge(nil); age != 0 {
		t.Fatalf("empty age = %v want 0", age)
	}
	now := time.Now().UTC()
	age := exchangeObservedQuoteAge([]exchange.Order{
		{ID: "new", CreatedAt: now.Add(-2 * time.Second)},
		{ID: "old", CreatedAt: now.Add(-5 * time.Second)},
		{ID: "zero"},
	})
	if age < 4*time.Second || age > 6*time.Second {
		t.Fatalf("age = %v want about 5s", age)
	}
}
