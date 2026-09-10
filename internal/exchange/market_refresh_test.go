package exchange

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// marketsServer serves /v1/markets with a taker fee the test can change between calls, and counts
// how many times it was asked. Everything below turns on when the client refetches and what it
// does when it cannot.
type marketsServer struct {
	takerFeeBps atomic.Int64
	calls       atomic.Int64
	fail        atomic.Bool
	empty       atomic.Bool
}

func (m *marketsServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.calls.Add(1)
		if m.fail.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		body := []map[string]any{}
		if !m.empty.Load() {
			body = append(body, map[string]any{
				"market":             "USDCcNGN-SPOT",
				"base_asset_symbol":  "USDC",
				"quote_asset_symbol": "cNGN",
				"asset_address":      "0x9d806fd040a719d27a8e5e77dc5ae0ed1e089493",
				"sub_id":             "0",
				"tick_size":          "0.000000000000000001",
				"order_entry_spec":   "usdc_cngn_spot_v1",
				"taker_fee_bps":      m.takerFeeBps.Load(),
			})
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newTestClient builds a client wired to the fake venue, skipping the RPC work NewHTTPClient does.
func newTestClient(t *testing.T, baseURL string, refresh time.Duration) *HTTPClient {
	t.Helper()
	return &HTTPClient{
		cfg:          ClientConfig{APIBaseURL: baseURL, MarketSymbol: "USDCcNGN-SPOT", WorstFee: "0"},
		httpClient:   &http.Client{Timeout: 5 * time.Second},
		markets:      make(map[string]MarketSpec),
		refreshEvery: refresh,
	}
}

// The defect: the schedule was read once at construction and never again, so a bot that started
// before a fee existed kept signing worstFee 0 forever.
func TestGetMarketPicksUpAScheduleThatAppearsAfterStartup(t *testing.T) {
	venue := &marketsServer{}
	venue.takerFeeBps.Store(0)
	srv := venue.start(t)
	c := newTestClient(t, srv.URL, time.Millisecond)

	spec, err := c.GetMarket(context.Background(), "USDCcNGN-SPOT")
	if err != nil {
		t.Fatalf("GetMarket: %v", err)
	}
	if spec.TakerFeeBps != 0 {
		t.Fatalf("taker fee = %d, want 0 before the venue publishes one", spec.TakerFeeBps)
	}

	// The venue turns fees on while the bot is running -- exactly the 2026-09-10 cutover.
	venue.takerFeeBps.Store(25)
	time.Sleep(5 * time.Millisecond)

	spec, err = c.GetMarket(context.Background(), "USDCcNGN-SPOT")
	if err != nil {
		t.Fatalf("GetMarket after the change: %v", err)
	}
	if spec.TakerFeeBps != 25 {
		t.Fatalf("taker fee = %d, want 25 -- the cache never refreshed, which is the whole bug", spec.TakerFeeBps)
	}
}

// And the bound derived from it must follow, since that is what actually reaches the chain.
func TestSignedBoundFollowsAScheduleChange(t *testing.T) {
	venue := &marketsServer{}
	venue.takerFeeBps.Store(0)
	srv := venue.start(t)
	c := newTestClient(t, srv.URL, time.Millisecond)

	spec, _ := c.GetMarket(context.Background(), "USDCcNGN-SPOT")
	before, err := c.signedWorstFee(spec, 1.0/1376.0)
	if err != nil {
		t.Fatalf("signedWorstFee: %v", err)
	}
	if before != "0" {
		t.Fatalf("bound = %s, want the configured fallback while no schedule is published", before)
	}

	venue.takerFeeBps.Store(25)
	time.Sleep(5 * time.Millisecond)
	spec, _ = c.GetMarket(context.Background(), "USDCcNGN-SPOT")
	after, err := c.signedWorstFee(spec, 1.0/1376.0)
	if err != nil {
		t.Fatalf("signedWorstFee after the change: %v", err)
	}
	if after == "0" {
		t.Fatal("bound is still the zero fallback after the venue published 25 bps -- every cross would revert")
	}
}

// A failed refresh must NOT regress to a zero TakerFeeBps. signedWorstFee reads zero as "no
// schedule published" and substitutes MM_WORST_FEE, so an RPC blip would silently reintroduce the
// zero bound and turn a transient error into a venue-wide revert loop.
func TestAFailedRefreshKeepsTheLastGoodSchedule(t *testing.T) {
	venue := &marketsServer{}
	venue.takerFeeBps.Store(25)
	srv := venue.start(t)
	c := newTestClient(t, srv.URL, time.Millisecond)

	if _, err := c.GetMarket(context.Background(), "USDCcNGN-SPOT"); err != nil {
		t.Fatalf("initial GetMarket: %v", err)
	}

	venue.fail.Store(true)
	time.Sleep(5 * time.Millisecond)

	spec, err := c.GetMarket(context.Background(), "USDCcNGN-SPOT")
	if err != nil {
		t.Fatalf("a failed refresh must not surface as an error while a good schedule is held: %v", err)
	}
	if spec.TakerFeeBps != 25 {
		t.Fatalf("taker fee = %d, want the last good 25", spec.TakerFeeBps)
	}
	if !c.MarketsStale() {
		t.Fatal("the schedule is stale and nothing recorded it")
	}

	// ...and recovers silently once the venue is back.
	venue.fail.Store(false)
	time.Sleep(5 * time.Millisecond)
	if _, err := c.GetMarket(context.Background(), "USDCcNGN-SPOT"); err != nil {
		t.Fatalf("GetMarket after recovery: %v", err)
	}
	if c.MarketsStale() {
		t.Fatal("still flagged stale after a successful refresh")
	}
}

// An empty response is a bad deploy far more often than a venue that delisted everything, so it
// must not wipe the schedule either.
func TestAnEmptyMarketsResponseDoesNotClearTheSchedule(t *testing.T) {
	venue := &marketsServer{}
	venue.takerFeeBps.Store(25)
	srv := venue.start(t)
	c := newTestClient(t, srv.URL, time.Millisecond)

	if _, err := c.GetMarket(context.Background(), "USDCcNGN-SPOT"); err != nil {
		t.Fatalf("initial GetMarket: %v", err)
	}
	venue.empty.Store(true)
	time.Sleep(5 * time.Millisecond)

	spec, err := c.GetMarket(context.Background(), "USDCcNGN-SPOT")
	if err != nil {
		t.Fatalf("GetMarket: %v", err)
	}
	if spec.TakerFeeBps != 25 {
		t.Fatalf("taker fee = %d, want the last good 25 rather than an empty schedule", spec.TakerFeeBps)
	}
}

// Never having loaded a schedule is different from holding a stale one: there is nothing safe to
// quote against, so it must be an error rather than a zero-fee guess.
func TestGetMarketFailsWhenNoScheduleWasEverLoaded(t *testing.T) {
	venue := &marketsServer{}
	venue.fail.Store(true)
	srv := venue.start(t)
	c := newTestClient(t, srv.URL, time.Millisecond)

	if _, err := c.GetMarket(context.Background(), "USDCcNGN-SPOT"); err == nil {
		t.Fatal("want an error when no schedule has ever been loaded")
	}
}

// The refresh is bounded, not per-call: one request a minute, not one per order.
func TestGetMarketServesTheCacheWithinTheRefreshWindow(t *testing.T) {
	venue := &marketsServer{}
	venue.takerFeeBps.Store(25)
	srv := venue.start(t)
	c := newTestClient(t, srv.URL, time.Hour)

	for i := 0; i < 20; i++ {
		if _, err := c.GetMarket(context.Background(), "USDCcNGN-SPOT"); err != nil {
			t.Fatalf("GetMarket: %v", err)
		}
	}
	if got := venue.calls.Load(); got != 1 {
		t.Fatalf("hit /v1/markets %d times for 20 reads inside the window, want 1", got)
	}
}

// The cache is now written on the quoting path while other calls read it. Run with -race.
func TestConcurrentGetMarketIsRaceFree(t *testing.T) {
	venue := &marketsServer{}
	venue.takerFeeBps.Store(25)
	srv := venue.start(t)
	c := newTestClient(t, srv.URL, time.Millisecond)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				if _, err := c.GetMarket(context.Background(), "USDCcNGN-SPOT"); err != nil {
					t.Errorf("goroutine %d: %v", n, err)
					return
				}
				c.marketSpecForAsset("0x9d806fd040a719d27a8e5e77dc5ae0ed1e089493", "0")
				_ = c.MarketsStale()
				venue.takerFeeBps.Store(int64(20 + j%10))
			}
		}(i)
	}
	wg.Wait()
}

func TestRefreshIntervalTreatsUnsetAsTheDefault(t *testing.T) {
	for _, tc := range []struct {
		seconds int64
		want    time.Duration
	}{
		{0, defaultRefreshEvery},  // unset -- must not mean "never refresh"
		{-5, defaultRefreshEvery}, // nonsense -- same
		{30, 30 * time.Second},
	} {
		if got := refreshInterval(tc.seconds); got != tc.want {
			t.Fatalf("refreshInterval(%d) = %v, want %v", tc.seconds, got, tc.want)
		}
	}
}
