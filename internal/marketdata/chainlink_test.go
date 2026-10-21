package marketdata

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gethmath "github.com/ethereum/go-ethereum/common/math"

	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/marketnames"
	"github.com/numofx/market-maker/internal/state"
)

// The selectors the feed is called with, as `cast sig` derives them: decimals() 0x313ce567,
// latestRoundData() 0xfeaf968c. The fake feed below dispatches on these literals, so an ABI that
// packed anything else would get "unknown selector" back and every test here would fail with it.
const (
	selectorDecimals        = "313ce567"
	selectorLatestRoundData = "feaf968c"
)

func TestAggregatorV3Selectors(t *testing.T) {
	if got := hex.EncodeToString(cngnOracleABI.Methods["decimals"].ID); got != selectorDecimals {
		t.Fatalf("decimals selector = %s, cast sig says %s", got, selectorDecimals)
	}
	if got := hex.EncodeToString(cngnOracleABI.Methods["latestRoundData"].ID); got != selectorLatestRoundData {
		t.Fatalf("latestRoundData selector = %s, cast sig says %s", got, selectorLatestRoundData)
	}
}

// fakeFeed is a Chainlink AggregatorV3 behind a JSON-RPC endpoint.
type fakeFeed struct {
	mu        sync.Mutex
	decimals  uint8
	answer    *big.Int
	updatedAt int64
	calls     atomic.Int64
}

func word(v *big.Int) string { return hex.EncodeToString(gethmath.U256Bytes(new(big.Int).Set(v))) }

func (f *fakeFeed) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     any               `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Method != "eth_call" || len(req.Params) == 0 {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var call struct {
			Data string `json:"data"`
		}
		_ = json.Unmarshal(req.Params[0], &call)
		f.calls.Add(1)
		f.mu.Lock()
		defer f.mu.Unlock()
		var result string
		switch strings.TrimPrefix(call.Data, "0x") {
		case selectorDecimals:
			result = "0x" + word(big.NewInt(int64(f.decimals)))
		case selectorLatestRoundData:
			round := big.NewInt(36893488147419103477 - 1<<65)
			result = "0x" + word(round) + word(f.answer) + word(big.NewInt(f.updatedAt)) + word(big.NewInt(f.updatedAt)) + word(round)
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32000, "message": "unknown selector " + call.Data}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeFeed) set(answer int64, updatedAt time.Time) {
	f.mu.Lock()
	f.answer = big.NewInt(answer)
	f.updatedAt = updatedAt.Unix()
	f.mu.Unlock()
}

const eurcUSDFeed = "0xDAe398520e2B67cd3f27aeF9Cf14D93D927f8250"

func newTestChainlink(srv *httptest.Server, maxAge time.Duration, now func() time.Time) *ChainlinkAnchorSource {
	s := NewChainlinkAnchorSource(srv.URL, eurcUSDFeed, maxAge)
	s.now = now
	return s
}

// The live EURC/USD round read on Base on 2026-10-21: 112465583 at 8 decimals is 1.12465583.
func TestChainlinkAnchorScalesTheAnswerByTheFeedsDecimals(t *testing.T) {
	now := time.Unix(1792576376, 0)
	for _, tc := range []struct {
		decimals uint8
		answer   int64
		want     float64
	}{
		{8, 112465583, 1.12465583},
		{18, 1124655830000000000, 1.12465583},
		{6, 1124655, 1.124655},
		{0, 2, 2},
	} {
		feed := &fakeFeed{decimals: tc.decimals}
		feed.set(tc.answer, now.Add(-70107*time.Second))
		src := newTestChainlink(feed.serve(t), 90000*time.Second, func() time.Time { return now })
		got, err := src.GetAnchorPrice(context.Background(), "EURC-USDC")
		if err != nil {
			t.Fatalf("decimals %d: %v", tc.decimals, err)
		}
		if math.Abs(got-tc.want) > 1e-12 {
			t.Fatalf("decimals %d answer %d = %v, want %v", tc.decimals, tc.answer, got, tc.want)
		}
	}
}

func TestChainlinkAnchorRefusesANonPositiveAnswer(t *testing.T) {
	now := time.Now()
	for _, answer := range []int64{0, -112465583} {
		feed := &fakeFeed{decimals: 8}
		feed.set(answer, now)
		src := newTestChainlink(feed.serve(t), time.Hour, func() time.Time { return now })
		price, err := src.GetAnchorPrice(context.Background(), "EURC-USDC")
		if !errors.Is(err, ErrAnchorInvalid) || price != 0 {
			t.Fatalf("answer %d: price %v err %v, want ErrAnchorInvalid", answer, price, err)
		}
	}
}

func TestChainlinkAnchorRefusesAStaleRound(t *testing.T) {
	now := time.Unix(1792576376, 0)
	feed := &fakeFeed{decimals: 8}
	feed.set(112465583, now.Add(-90001*time.Second))
	src := newTestChainlink(feed.serve(t), 90000*time.Second, func() time.Time { return now })
	if price, err := src.GetAnchorPrice(context.Background(), "EURC-USDC"); !errors.Is(err, ErrAnchorStale) || price != 0 {
		t.Fatalf("a round 90001s old against a 90000s max age: price %v err %v, want ErrAnchorStale", price, err)
	}

	// An unset updatedAt is an incomplete round, not a fresh one.
	feed.set(112465583, time.Unix(0, 0))
	src = newTestChainlink(feed.serve(t), 90000*time.Second, func() time.Time { return now })
	if _, err := src.GetAnchorPrice(context.Background(), "EURC-USDC"); !errors.Is(err, ErrAnchorStale) {
		t.Fatalf("updatedAt 0: err %v, want ErrAnchorStale", err)
	}
}

// The read is throttled, but the round's age is not: a cached round that crosses the max age between
// reads is refused on the call where it does, without waiting for the next read.
func TestChainlinkAnchorAgesACachedRound(t *testing.T) {
	start := time.Unix(1792576376, 0)
	clock := start
	feed := &fakeFeed{decimals: 8}
	feed.set(112465583, start.Add(-89995*time.Second))
	src := newTestChainlink(feed.serve(t), 90000*time.Second, func() time.Time { return clock })
	if _, err := src.GetAnchorPrice(context.Background(), "EURC-USDC"); err != nil {
		t.Fatalf("fresh round: %v", err)
	}
	reads := feed.calls.Load()
	clock = start.Add(10 * time.Second) // inside the 15s throttle
	if _, err := src.GetAnchorPrice(context.Background(), "EURC-USDC"); !errors.Is(err, ErrAnchorStale) {
		t.Fatalf("cached round now 90005s old: err %v, want ErrAnchorStale", err)
	}
	if feed.calls.Load() != reads {
		t.Fatalf("the throttled call re-read the feed (%d -> %d calls)", reads, feed.calls.Load())
	}
}

// An RPC that cannot be read is a plain error, left to the MM_STALE_ANCHOR_TIMEOUT_SECONDS grace.
func TestChainlinkAnchorRPCFailureIsNotAStaleRound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream down", http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	src := newTestChainlink(srv, time.Hour, time.Now)
	_, err := src.GetAnchorPrice(context.Background(), "EURC-USDC")
	if err == nil || errors.Is(err, ErrAnchorStale) || errors.Is(err, ErrAnchorInvalid) {
		t.Fatalf("rpc 502: err %v, want a plain error", err)
	}
}

// eurcClient is the exchange client a EURC-USDC bot sees, with cNGN-USDC listed beside it.
type eurcClient struct {
	exchange.Client
	book exchange.Book
}

func (c eurcClient) GetBook(context.Context, string) (exchange.Book, error) { return c.book, nil }
func (eurcClient) GetTrades(context.Context, string) ([]exchange.Trade, error) {
	return nil, nil
}
func (eurcClient) GetBalances(context.Context) ([]exchange.Balance, error) { return nil, nil }
func (eurcClient) ListOpenOrders(context.Context, string) ([]exchange.Order, error) {
	return nil, nil
}
func (eurcClient) SpotMarket() (string, bool) { return marketnames.SpotCanonical, true }

type countingAnchor struct{ calls int }

func (a *countingAnchor) Name() string { return "chainlink" }
func (a *countingAnchor) GetAnchorPrice(context.Context, string) (float64, error) {
	a.calls++
	return 1.1246, nil
}

var eurcSpec = exchange.MarketSpec{Symbol: "EURC-USDC", Kind: exchange.MarketKindSpot, BaseAsset: "EURC", QuoteAsset: "USDC"}

// EURC-USDC is not the cNGN spot market: its snapshot reads the generic anchor as its AnchorPrice
// (what the deviation and stale-anchor guards hold the book to), never the cNGN spot path's
// bootstrap-only oracle, and the cNGN spot pricing does not apply to it.
func TestAnchoredSpotLoaderReadsTheAnchorAsItsGuard(t *testing.T) {
	anchor := &countingAnchor{}
	loader := NewLoader(eurcClient{}, eurcSpec, anchor)
	if got := loader.SpotMarket(); got != marketnames.SpotCanonical {
		t.Fatalf("spot reference market = %q, want the cNGN spot market", got)
	}
	snap, err := loader.Load(context.Background(), state.Snapshot{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if anchor.calls != 1 || snap.AnchorPrice != 1.1246 || snap.AnchorSource != "chainlink" || snap.LastAnchorRefresh.IsZero() {
		t.Fatalf("anchor calls %d, snapshot anchor %v from %q at %v", anchor.calls, snap.AnchorPrice, snap.AnchorSource, snap.LastAnchorRefresh)
	}
	if snap.IsCNGNSpotMarket() {
		t.Fatal("a EURC-USDC snapshot reads as the cNGN spot market")
	}
}

// Regression: the cNGN spot market keeps its own path. Its oracle is never its AnchorPrice and the
// generic anchor is not read, so the deviation guard cannot halt it.
func TestCNGNSpotLoaderStillSkipsTheGenericAnchor(t *testing.T) {
	anchor := &countingAnchor{}
	spec := exchange.MarketSpec{Symbol: marketnames.SpotCanonical, Kind: exchange.MarketKindSpot, BaseAsset: "cNGN", QuoteAsset: "USDC"}
	snap, err := NewLoader(eurcClient{}, spec, anchor).Load(context.Background(), state.Snapshot{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if anchor.calls != 0 || snap.AnchorPrice != 0 || !snap.IsCNGNSpotMarket() {
		t.Fatalf("cNGN spot: anchor calls %d, AnchorPrice %v, is cNGN spot %v", anchor.calls, snap.AnchorPrice, snap.IsCNGNSpotMarket())
	}
}
