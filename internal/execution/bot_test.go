package execution

import (
	"testing"
	"time"

	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/state"
)

func TestMarketMetadataAttrsIncludesExecutionMetadata(t *testing.T) {
	spec := exchange.MarketSpec{
		Symbol:         "USDCcNGN-SPOT",
		BaseAsset:      "USDC",
		QuoteAsset:     "cNGN",
		AssetAddress:   "0xe4b6e05b9910ab08a947a20faecc4524bf8a7f7e",
		QuoteAddress:   "0x1917960763bf3a0dfa10a05f0a112e828c1a934f",
		SubID:          "0",
		TickSize:       0.01,
		SizeStep:       0.000001,
		MinSize:        0.000001,
		OrderEntrySpec: "usdc_cngn_spot_v1",
	}
	attrs := marketMetadataAttrs(spec)
	values := map[string]any{}
	for i := 0; i+1 < len(attrs); i += 2 {
		values[attrs[i].(string)] = attrs[i+1]
	}
	for key, want := range map[string]any{
		"market":           spec.Symbol,
		"asset_address":    spec.AssetAddress,
		"quote_address":    spec.QuoteAddress,
		"sub_id":           spec.SubID,
		"size_step":        spec.SizeStep,
		"min_size":         spec.MinSize,
		"order_entry_spec": spec.OrderEntrySpec,
	} {
		if values[key] != want {
			t.Fatalf("%s = %#v want %#v", key, values[key], want)
		}
	}
}

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
