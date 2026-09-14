package execution

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/metrics"
	"github.com/numofx/market-maker/internal/state"
	"github.com/numofx/market-maker/internal/strategy"
)

// goneClient answers a cancel the way the venue does for an order that filled or was reserved by the
// matcher between the list and the cancel.
type goneClient struct {
	mockClient
	gone map[string]bool
}

func (g *goneClient) CancelOrder(ctx context.Context, orderID string, reason string) error {
	if g.gone[orderID] {
		return fmt.Errorf("%w: POST /v1/orders/cancel: 404", exchange.ErrOrderNotFound)
	}
	return g.mockClient.CancelOrder(ctx, orderID, reason)
}

// With a 60s expiry every quote is cancelled about every 45s, so "it filled just before the roll" is
// routine. It must not abort the cycle: the freed slot gets its replacement, and the other side still
// rolls in the same Sync.
func TestAFillRacingTheExpiryRollDoesNotAbortTheCycle(t *testing.T) {
	client := &goneClient{mockClient: mockClient{requiredWorstFee: "1000"}, gone: map[string]bool{"bid-filled": true}}
	syncer := NewSyncer(client, exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", MinSize: 0.000001}, expiryCfg(), metrics.New(),
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	soon := time.Now().UTC().Unix() + 5
	bid := restingAtTarget("bid-filled", soon)
	ask := restingAtTarget("ask-rolls", soon)
	ask.Side, ask.Price = exchange.SideSell, 1330

	result, err := syncer.Sync(context.Background(),
		state.Snapshot{Market: "USDCcNGN-SPOT", OpenOrders: []exchange.Order{bid, ask}},
		strategy.Result{
			Bids: []strategy.Quote{{Side: exchange.SideBuy, Price: 1326, Size: 1.2}},
			Asks: []strategy.Quote{{Side: exchange.SideSell, Price: 1330, Size: 1.2}},
		},
		map[exchange.Side][]Identity{
			exchange.SideBuy:  {{OrderID: "bid-new", Nonce: "1"}},
			exchange.SideSell: {{OrderID: "ask-new", Nonce: "2"}},
		})
	if err != nil {
		t.Fatalf("sync aborted on a not-found cancel: %v", err)
	}
	placed := map[string]bool{}
	for _, p := range client.placed {
		placed[p.OrderID] = true
	}
	if !placed["bid-new"] {
		t.Fatalf("placed %+v, want the filled bid's slot re-quoted", client.placed)
	}
	if !placed["ask-new"] || len(client.cancelled) != 1 || client.cancelled[0] != "ask-rolls" {
		t.Fatalf("cancelled %v placed %+v, want the ask rolled in the same cycle", client.cancelled, client.placed)
	}
	if !result.Changed {
		t.Fatal("re-quoting is a change")
	}
}

// Any other cancel failure still stops the cycle rather than stacking a replacement on top of an
// order that may still be resting.
func TestOtherCancelErrorsStillAbortTheCycle(t *testing.T) {
	client := &failingCancelClient{mockClient: mockClient{requiredWorstFee: "1000"}}
	syncer := NewSyncer(client, exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", MinSize: 0.000001}, expiryCfg(), metrics.New(),
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	_, err := syncer.Sync(context.Background(),
		state.Snapshot{Market: "USDCcNGN-SPOT", OpenOrders: []exchange.Order{restingAtTarget("bid", time.Now().UTC().Unix()+5)}},
		strategy.Result{Bids: []strategy.Quote{{Side: exchange.SideBuy, Price: 1326, Size: 1.2}}},
		map[exchange.Side][]Identity{exchange.SideBuy: {{OrderID: "bid-new", Nonce: "1"}}})
	if err == nil {
		t.Fatal("a failed cancel was swallowed")
	}
	if len(client.placed) != 0 {
		t.Fatalf("placed %+v on top of an order whose cancel failed", client.placed)
	}
}

type failingCancelClient struct{ mockClient }

func (f *failingCancelClient) CancelOrder(context.Context, string, string) error {
	return fmt.Errorf("POST /v1/orders/cancel: 500 failed to cancel order")
}
