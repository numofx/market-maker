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

// postOnlyClient records what was actually sent, and can refuse like the venue does.
type postOnlyClient struct {
	mockClient
	rejectAll bool
	requests  []exchange.PlaceOrderRequest
}

func (p *postOnlyClient) PlaceLimitOrder(_ context.Context, req exchange.PlaceOrderRequest) (exchange.Order, error) {
	p.requests = append(p.requests, req)
	if p.rejectAll {
		return exchange.Order{}, exchange.ErrPostOnlyWouldCross
	}
	p.placed = append(p.placed, req)
	return exchange.Order{ID: req.OrderID, Nonce: req.Nonce, Side: req.Side, Price: req.Price, Size: req.Size}, nil
}

func postOnlySyncer(client exchange.Client, postOnly bool) *Syncer {
	return NewSyncer(client, exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", MinSize: 0.000001},
		config.Config{CancelStaleOrderThreshold: 10, AdoptSizeTolerance: 0.000001, PostOnlyQuotes: postOnly},
		metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func ladder() strategy.Result {
	return strategy.Result{
		Bids: []strategy.Quote{
			{Side: exchange.SideBuy, Price: 1325, Size: 1.2},
			{Side: exchange.SideBuy, Price: 1323, Size: 1.44},
		},
		Asks: []strategy.Quote{
			{Side: exchange.SideSell, Price: 1328, Size: 1.2},
			{Side: exchange.SideSell, Price: 1330, Size: 1.44},
		},
	}
}

func ladderIdentities() map[exchange.Side][]Identity {
	return map[exchange.Side][]Identity{
		exchange.SideBuy:  {{OrderID: "b1", Nonce: "1"}, {OrderID: "b2", Nonce: "2"}},
		exchange.SideSell: {{OrderID: "s1", Nonce: "3"}, {OrderID: "s2", Nonce: "4"}},
	}
}

// Every quote must carry the flag. One level without it is one level that can take, and since the
// fee follows whichever order arrived later, that is the level that pays the taker fee.
func TestEveryQuoteCarriesPostOnly(t *testing.T) {
	client := &postOnlyClient{}
	syncer := postOnlySyncer(client, true)

	if _, err := syncer.Sync(context.Background(),
		state.Snapshot{Market: "USDCcNGN-SPOT"}, ladder(), ladderIdentities()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(client.requests) != 4 {
		t.Fatalf("placed %d orders, want the whole 2x2 ladder", len(client.requests))
	}
	for _, req := range client.requests {
		if !req.PostOnly {
			t.Fatalf("%s quote at %v went without post_only -- it can take", req.Side, req.Price)
		}
	}
}

// The opt-out has to actually opt out, or the flag is unfalsifiable.
func TestPostOnlyCanBeTurnedOff(t *testing.T) {
	client := &postOnlyClient{}
	syncer := postOnlySyncer(client, false)

	if _, err := syncer.Sync(context.Background(),
		state.Snapshot{Market: "USDCcNGN-SPOT"}, ladder(), ladderIdentities()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	for _, req := range client.requests {
		if req.PostOnly {
			t.Fatal("post_only was sent with MM_POST_ONLY_QUOTES=false")
		}
	}
}

// The behaviour that matters when the guard fires. A rejection propagates out of place() into
// reconcileSide into Sync, and RunCycle returns a bare error -- so left unhandled, one quote
// priced a tick too aggressively would abandon every remaining level on BOTH sides.
func TestARejectedQuoteDoesNotAbandonTheRestOfTheLadder(t *testing.T) {
	client := &postOnlyClient{rejectAll: true}
	syncer := postOnlySyncer(client, true)

	_, err := syncer.Sync(context.Background(),
		state.Snapshot{Market: "USDCcNGN-SPOT"}, ladder(), ladderIdentities())
	if err != nil {
		t.Fatalf("a post-only rejection must not fail the cycle: %v", err)
	}
	if len(client.requests) != 4 {
		t.Fatalf("attempted %d levels, want all 4 -- the first rejection stopped the ladder",
			len(client.requests))
	}
	if got := syncer.metrics.PostOnlyRejections(); got != 4 {
		t.Fatalf("counted %d rejections, want 4", got)
	}
}

// A rejection is not an error, and must not be counted as one -- an error rate that climbs on a
// fast market would page someone for the guard working correctly.
func TestARejectionIsNotCountedAsAnError(t *testing.T) {
	client := &postOnlyClient{rejectAll: true}
	syncer := postOnlySyncer(client, true)

	before := syncer.metrics.Errors()
	if _, err := syncer.Sync(context.Background(),
		state.Snapshot{Market: "USDCcNGN-SPOT"}, ladder(), ladderIdentities()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if after := syncer.metrics.Errors(); after != before {
		t.Fatalf("errors went %d -> %d; a post-only rejection is the guard working", before, after)
	}
}

// A genuine failure must still be a failure -- the skip is for the post-only case only, and a
// blanket "ignore place errors" would hide a venue that is simply down.
func TestAnOrdinaryPlaceFailureStillFailsTheCycle(t *testing.T) {
	client := &mockClient{placeErr: context.DeadlineExceeded}
	syncer := postOnlySyncer(client, true)

	if _, err := syncer.Sync(context.Background(),
		state.Snapshot{Market: "USDCcNGN-SPOT"}, ladder(), ladderIdentities()); err == nil {
		t.Fatal("an ordinary place failure must still fail the cycle")
	}
}
