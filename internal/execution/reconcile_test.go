package execution

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"testing"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/state"
	"github.com/numofx/market-maker/internal/strategy"
)

func TestReconcileStartup(t *testing.T) {
	spec := exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "USDC", QuoteAsset: "cNGN"}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{
		CancelStaleOrderThreshold: 5,
		AdoptSizeTolerance:        0.01,
		MaxLongInventory:          100,
		MaxShortInventory:         -100,
		MinBaseBalance:            10,
	}
	baseSnapshot := state.Snapshot{
		Market:           spec.Symbol,
		ReferencePrice:   100,
		InventoryByAsset: map[string]float64{"USDC": 0},
		Positions: map[string]state.AssetPosition{
			"USDC": {Available: 50, Total: 50},
			"cNGN": {Available: 10000, Total: 10000},
		},
	}
	quotes := strategy.Result{
		ReferencePrice: 100,
		Bid:            &strategy.Quote{Side: exchange.SideBuy, Price: 99.5, Size: 10},
		Ask:            &strategy.Quote{Side: exchange.SideSell, Price: 100.5, Size: 10},
	}

	tests := []struct {
		name         string
		snapshot     state.Snapshot
		quotes       strategy.Result
		wantAdoptBid string
		wantAdoptAsk string
		wantCanceled []string
		wantRejects  map[string]string
	}{
		{
			name: "exactly matching bid ask both adopted",
			snapshot: withOrders(baseSnapshot, []exchange.Order{
				{ID: "mm:USDCcNGN-SPOT:buy:10", Side: exchange.SideBuy, Price: 99.5, Size: 10, Managed: true},
				{ID: "mm:USDCcNGN-SPOT:sell:11", Side: exchange.SideSell, Price: 100.5, Size: 10, Managed: true},
			}),
			quotes:       quotes,
			wantAdoptBid: "mm:USDCcNGN-SPOT:buy:10",
			wantAdoptAsk: "mm:USDCcNGN-SPOT:sell:11",
			wantCanceled: []string{},
			wantRejects:  map[string]string{},
		},
		{
			name: "only bid adopted ask canceled replaced",
			snapshot: withOrders(baseSnapshot, []exchange.Order{
				{ID: "mm:USDCcNGN-SPOT:buy:10", Side: exchange.SideBuy, Price: 99.5, Size: 10, Managed: true},
				{ID: "mm:USDCcNGN-SPOT:sell:11", Side: exchange.SideSell, Price: 102, Size: 10, Managed: true},
			}),
			quotes:       quotes,
			wantAdoptBid: "mm:USDCcNGN-SPOT:buy:10",
			wantCanceled: []string{"mm:USDCcNGN-SPOT:sell:11"},
			wantRejects:  map[string]string{"mm:USDCcNGN-SPOT:sell:11": "price_too_far_from_target"},
		},
		{
			name: "duplicate bids cause cancel",
			snapshot: withOrders(baseSnapshot, []exchange.Order{
				{ID: "mm:USDCcNGN-SPOT:buy:10", Side: exchange.SideBuy, Price: 99.5, Size: 10, Managed: true},
				{ID: "mm:USDCcNGN-SPOT:buy:12", Side: exchange.SideBuy, Price: 99.5, Size: 10, Managed: true},
			}),
			quotes:       quotes,
			wantCanceled: []string{"mm:USDCcNGN-SPOT:buy:10", "mm:USDCcNGN-SPOT:buy:12"},
			wantRejects: map[string]string{
				"mm:USDCcNGN-SPOT:buy:10": "duplicate_side",
				"mm:USDCcNGN-SPOT:buy:12": "duplicate_side",
			},
		},
		{
			name: "malformed metadata causes cancel",
			snapshot: withOrders(baseSnapshot, []exchange.Order{
				{ID: "mm:USDCcNGN-SPOT:oops", Side: exchange.SideBuy, Price: 99.5, Size: 10, Managed: true},
			}),
			quotes:       quotes,
			wantCanceled: []string{"mm:USDCcNGN-SPOT:oops"},
			wantRejects:  map[string]string{"mm:USDCcNGN-SPOT:oops": "malformed_metadata"},
		},
		{
			name: "price drift beyond threshold causes cancel",
			snapshot: withOrders(baseSnapshot, []exchange.Order{
				{ID: "mm:USDCcNGN-SPOT:buy:10", Side: exchange.SideBuy, Price: 98, Size: 10, Managed: true},
			}),
			quotes:       quotes,
			wantCanceled: []string{"mm:USDCcNGN-SPOT:buy:10"},
			wantRejects:  map[string]string{"mm:USDCcNGN-SPOT:buy:10": "price_too_far_from_target"},
		},
		{
			name: "size mismatch beyond tolerance causes cancel",
			snapshot: withOrders(baseSnapshot, []exchange.Order{
				{ID: "mm:USDCcNGN-SPOT:buy:10", Side: exchange.SideBuy, Price: 99.5, Size: 12, Managed: true},
			}),
			quotes:       quotes,
			wantCanceled: []string{"mm:USDCcNGN-SPOT:buy:10"},
			wantRejects:  map[string]string{"mm:USDCcNGN-SPOT:buy:10": "size_too_far_from_target"},
		},
		{
			name: "ambiguous ownership causes cancel",
			snapshot: withOrders(baseSnapshot, []exchange.Order{
				{ID: "manual-order", Side: exchange.SideBuy, Price: 99.5, Size: 10, Managed: false},
			}),
			quotes:       quotes,
			wantCanceled: []string{"manual-order"},
			wantRejects:  map[string]string{"manual-order": "ambiguous_ownership"},
		},
		{
			name: "risk halt on startup cancels all rather than adopting",
			snapshot: withOrders(state.Snapshot{
				Market:           spec.Symbol,
				ReferencePrice:   100,
				InventoryByAsset: map[string]float64{"USDC": 0},
				Positions: map[string]state.AssetPosition{
					"USDC": {Available: 1, Total: 1},
					"cNGN": {Available: 10, Total: 10},
				},
			}, []exchange.Order{
				{ID: "mm:USDCcNGN-SPOT:buy:10", Side: exchange.SideBuy, Price: 99.5, Size: 10, Managed: true},
				{ID: "mm:USDCcNGN-SPOT:sell:11", Side: exchange.SideSell, Price: 100.5, Size: 10, Managed: true},
			}),
			quotes:       quotes,
			wantCanceled: []string{"mm:USDCcNGN-SPOT:buy:10", "mm:USDCcNGN-SPOT:sell:11"},
			wantRejects: map[string]string{
				"mm:USDCcNGN-SPOT:buy:10":  "risk_halt:available base balance below threshold for USDC",
				"mm:USDCcNGN-SPOT:sell:11": "risk_halt:available base balance below threshold for USDC",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &mockClient{openOrders: tt.snapshot.OpenOrders}
			result, adopted, err := ReconcileStartup(context.Background(), client, cfg, spec, tt.snapshot, tt.quotes, logger)
			if err != nil {
				t.Fatalf("ReconcileStartup() error = %v", err)
			}
			if result.AdoptedBidOrderID != tt.wantAdoptBid {
				t.Fatalf("adopted bid = %q want %q", result.AdoptedBidOrderID, tt.wantAdoptBid)
			}
			if result.AdoptedAskOrderID != tt.wantAdoptAsk {
				t.Fatalf("adopted ask = %q want %q", result.AdoptedAskOrderID, tt.wantAdoptAsk)
			}
			sort.Strings(tt.wantCanceled)
			if !reflect.DeepEqual(result.CanceledOrderIDs, tt.wantCanceled) {
				t.Fatalf("canceled ids = %#v want %#v", result.CanceledOrderIDs, tt.wantCanceled)
			}
			if !reflect.DeepEqual(result.RejectedReasons, tt.wantRejects) {
				t.Fatalf("reject reasons = %#v want %#v", result.RejectedReasons, tt.wantRejects)
			}
			if len(adopted) != btoi(tt.wantAdoptBid != "")+btoi(tt.wantAdoptAsk != "") {
				t.Fatalf("adopted len = %d", len(adopted))
			}
		})
	}
}

func withOrders(snapshot state.Snapshot, orders []exchange.Order) state.Snapshot {
	snapshot.OpenOrders = orders
	return snapshot
}

func btoi(v bool) int {
	if v {
		return 1
	}
	return 0
}
