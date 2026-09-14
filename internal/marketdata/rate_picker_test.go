package marketdata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/numofx/market-maker/internal/config"
)

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func clientFunc(fn func(*http.Request) *http.Response) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) { return fn(req), nil })}
}

func assertPrice(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("price = %.9f, want %.9f", got, want)
	}
}

var ratePickerNow = time.UnixMilli(1_789_425_900_000)

func minutesAgo(m float64) time.Time {
	return ratePickerNow.Add(-time.Duration(m * float64(time.Minute)))
}

// Live on 2026-09-14 Quidax's usdtcngn served a candle every minute at 1371.8 with zero volume for the whole
// hour. A zero-volume candle repeats the last close; only candles that traded count.
func TestQuidaxAveragesOnlyCandlesThatTraded(t *testing.T) {
	var gotURL string
	body := fmt.Sprintf(`{"status":"success","data":[[%d,"9999","9999","9999","9999","0"],[%d,"1370.7","1370.7","1370.7","1370.7","225.5128"],[%d,"1360","1360","1360","1360","10"]]}`,
		minutesAgo(1).UnixMilli(), minutesAgo(2).UnixMilli(), minutesAgo(30).UnixMilli())
	source := &quidaxRateSource{
		client:       clientFunc(func(req *http.Request) *http.Response { gotURL = req.URL.String(); return jsonResponse(200, body) }),
		baseURL:      "https://quidax.test/api/v1",
		market:       "usdtngn",
		window:       time.Hour,
		maxStaleness: 6 * time.Hour,
	}

	price, err := source.PriceInNGN(context.Background(), ratePickerNow)
	if err != nil {
		t.Fatalf("PriceInNGN() error = %v", err)
	}
	// 1360 stood from 30m to 2m ago, 1370.7 for the last 2 minutes; the 9999 candle never traded.
	assertPrice(t, price, (1360*28+1370.7*2)/30)
	if gotURL != "https://quidax.test/api/v1/markets/usdtngn/k?period=1&limit=361" {
		t.Fatalf("url = %s", gotURL)
	}
}

func TestQuidaxRefusesAMarketThatStoppedTrading(t *testing.T) {
	for name, body := range map[string]string{
		"only zero-volume candles": fmt.Sprintf(`{"data":[[%d,"1371.8","1371.8","1371.8","1371.8","0"]]}`, minutesAgo(1).UnixMilli()),
		"last trade too old":       fmt.Sprintf(`{"data":[[%d,"1477","1477","1477","1477","5"]]}`, minutesAgo(7*60).UnixMilli()),
		"empty series":             `{"status":"success","data":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			source := &quidaxRateSource{client: clientFunc(func(*http.Request) *http.Response { return jsonResponse(200, body) }), baseURL: "https://quidax.test", market: "usdtcngn", window: time.Hour, maxStaleness: 6 * time.Hour}
			if price, err := source.PriceInNGN(context.Background(), ratePickerNow); err == nil {
				t.Fatalf("price = %v, want an error", price)
			}
		})
	}
}

func TestQuidaxUsesTheNewestTradeWhenTheWindowIsQuiet(t *testing.T) {
	body := fmt.Sprintf(`{"data":[[%d,"1368","1368","1368","1368","3"]]}`, minutesAgo(90).UnixMilli())
	source := &quidaxRateSource{client: clientFunc(func(*http.Request) *http.Response { return jsonResponse(200, body) }), baseURL: "https://quidax.test", market: "usdtngn", window: time.Hour, maxStaleness: 6 * time.Hour}
	price, err := source.PriceInNGN(context.Background(), ratePickerNow)
	if err != nil {
		t.Fatalf("PriceInNGN() error = %v", err)
	}
	assertPrice(t, price, 1368)
}

func TestTextileAveragesClearedTradesThenFallsBackToTheBook(t *testing.T) {
	cases := []struct {
		name   string
		trades string
		ticker string
		want   float64
	}{
		{
			name:   "trades in the window",
			trades: fmt.Sprintf(`{"buy":[{"price":"1372","trade_timestamp":%d}],"sell":[{"price":"1374","trade_timestamp":%d}]}`, minutesAgo(40).Unix(), minutesAgo(10).Unix()),
			want:   (1372*30 + 1374*10) / 40.0,
		},
		{
			// Live USDT_NGN on 2026-09-14: nothing cleared in the hour, book 1372.53 / 1373.08.
			name:   "no trades uses the bid/ask mid",
			trades: `{"buy":[],"sell":[]}`,
			ticker: `[{"ticker_id":"USDT_NGN","bid":"1372.53","ask":"1373.08","last_price":"1373.06"}]`,
			want:   (1372.53 + 1373.08) / 2,
		},
		{
			// Live USDC_NGN the same day: no bid, so the mid falls back to the last cleared price.
			name:   "one-sided book uses the last price",
			trades: `{"buy":[],"sell":[]}`,
			ticker: `[{"ticker_id":"USDT_NGN","bid":"0","ask":"1373.63","last_price":"1373.63"}]`,
			want:   1373.63,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			var tradesURL string
			source := &textileRateSource{
				client: clientFunc(func(req *http.Request) *http.Response {
					if req.URL.Path == "/historical_trades" {
						tradesURL = req.URL.String()
						return jsonResponse(200, tt.trades)
					}
					return jsonResponse(200, tt.ticker)
				}),
				baseURL:  "https://textile.test",
				tickerID: "USDT_NGN",
				window:   time.Hour,
				limit:    200,
			}
			price, err := source.PriceInNGN(context.Background(), ratePickerNow)
			if err != nil {
				t.Fatalf("PriceInNGN() error = %v", err)
			}
			assertPrice(t, price, tt.want)
			if want := fmt.Sprintf("start_time=%d", ratePickerNow.Add(-time.Hour).Unix()); !strings.Contains(tradesURL, want) {
				t.Fatalf("trades url %s missing %s", tradesURL, want)
			}
		})
	}
}

func TestTextileUnknownPairFails(t *testing.T) {
	source := &textileRateSource{client: clientFunc(func(*http.Request) *http.Response { return jsonResponse(404, `{"error":"unknown ticker"}`) }), baseURL: "https://textile.test", tickerID: "XXX_NGN", window: time.Hour, limit: 200}
	if _, err := source.PriceInNGN(context.Background(), ratePickerNow); err == nil {
		t.Fatal("expected an error for an unknown pair")
	}
}

func bybitAd(price string, orders, rate int, online bool) map[string]any {
	return map[string]any{"price": price, "recentOrderNum": orders, "recentExecuteRate": rate, "isOnline": online}
}

func TestBybitP2PTakesTheMidOfModalReputableAds(t *testing.T) {
	sides := map[string][]map[string]any{
		// Buy ads (the ask): the offline 1500 and the 5-order 1300 are not reputable.
		"1": {bybitAd("1372.20", 200, 98, true), bybitAd("1372.40", 150, 100, true), bybitAd("1371.90", 400, 95, true), bybitAd("1380.00", 300, 99, true), bybitAd("1500", 900, 100, false), bybitAd("1300", 5, 100, true)},
		// Sell ads (the bid): 0.5 is a fractional completion rate below 0.9; 1320 is more than 2% off the median.
		"0": {bybitAd("1369.80", 120, 97, true), bybitAd("1370.10", 130, 96, true), bybitAd("1369.60", 500, 99, true), bybitAd("1320", 800, 99, true), {"price": "1370", "recentOrderNum": 999, "recentExecuteRate": 0.5, "isOnline": true}},
	}
	source := &bybitP2PRateSource{
		client: clientFunc(func(req *http.Request) *http.Response {
			if !strings.HasPrefix(req.UserAgent(), "Mozilla/5.0") {
				// The live endpoint never answers a non-browser User-Agent.
				return jsonResponse(599, `{}`)
			}
			var payload struct {
				Side string `json:"side"`
			}
			raw, _ := io.ReadAll(req.Body)
			_ = json.Unmarshal(raw, &payload)
			items, _ := json.Marshal(sides[payload.Side])
			return jsonResponse(200, `{"ret_code":0,"ret_msg":"SUCCESS","result":{"items":`+string(items)+`}}`)
		}),
		baseURL: "https://bybit.test", asset: "USDT", pageSize: 50,
		minCompletedOrders: 100, minCompletionRate: 0.9, maxAvgReleaseSeconds: 900, maxDeviationFromMedian: 0.02,
	}

	price, err := source.PriceInNGN(context.Background(), ratePickerNow)
	if err != nil {
		t.Fatalf("PriceInNGN() error = %v", err)
	}
	// Ask mode 1372 (three ads round to it), bid mode 1370.
	assertPrice(t, price, 1371)
}

func TestBybitP2PNeedsThreeReputableAds(t *testing.T) {
	source := &bybitP2PRateSource{minCompletedOrders: 100, minCompletionRate: 0.9, maxAvgReleaseSeconds: 900, maxDeviationFromMedian: 0.02}
	ads := []bybitP2PAd{{price: 1372, completed: 200, completionRate: 1, online: true}, {price: 1372, completed: 200, completionRate: 1, online: true}}
	if _, err := source.modalPrice(ads, "buy"); err == nil {
		t.Fatal("expected an error with only two reputable ads")
	}
}

type fakeRateSource struct {
	name  string
	price float64
	err   error
	calls atomic.Int32
}

func (f *fakeRateSource) Name() string { return f.name }
func (f *fakeRateSource) PriceInNGN(context.Context, time.Time) (float64, error) {
	f.calls.Add(1)
	return f.price, f.err
}

func TestRatePickerUsesTheFirstSuccessInPriorityOrder(t *testing.T) {
	quidax := &fakeRateSource{name: "quidax", err: errors.New("down")}
	textile := &fakeRateSource{name: "textile", price: 1372.805}
	bybit := &fakeRateSource{name: "bybit-p2p", price: 1371}
	picker := &ratePicker{sources: []rateSource{quidax, textile, bybit}, timeout: time.Second, now: func() time.Time { return ratePickerNow }, health: map[string]*rateSourceHealth{}}

	used, all, err := picker.Pick(context.Background())
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if used.Provider != "textile" || used.Price != 1372.805 {
		t.Fatalf("used = %+v, want textile at 1372.805", used)
	}
	if len(all) != 3 || all[2].Price != 1371 || bybit.calls.Load() != 1 {
		t.Fatalf("every source should be queried and reported, got %+v", all)
	}
}

func TestRatePickerFailsWhenEverySourceFails(t *testing.T) {
	picker := &ratePicker{
		sources: []rateSource{&fakeRateSource{name: "quidax", err: errors.New("stale")}, &fakeRateSource{name: "textile", price: math.NaN()}},
		timeout: time.Second, now: func() time.Time { return ratePickerNow }, health: map[string]*rateSourceHealth{},
	}
	_, _, err := picker.Pick(context.Background())
	if err == nil || !strings.Contains(err.Error(), "quidax: stale") || !strings.Contains(err.Error(), "textile: invalid price") {
		t.Fatalf("error = %v, want both sources named", err)
	}
}

func TestRatePickerSkipsASourceAfterThreeFailuresUntilCooldown(t *testing.T) {
	clock := ratePickerNow
	failing := &fakeRateSource{name: "quidax", err: errors.New("down")}
	picker := &ratePicker{sources: []rateSource{failing, &fakeRateSource{name: "textile", price: 1372}}, timeout: time.Second, now: func() time.Time { return clock }, health: map[string]*rateSourceHealth{}}

	for i := 0; i < 4; i++ {
		if _, _, err := picker.Pick(context.Background()); err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
	}
	if got := failing.calls.Load(); got != 3 {
		t.Fatalf("failing source called %d times, want 3 before the circuit opens", got)
	}
	clock = clock.Add(ratePickerBreakerCooldown)
	if _, _, err := picker.Pick(context.Background()); err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got := failing.calls.Load(); got != 4 {
		t.Fatalf("failing source called %d times, want a trial call after the cooldown", got)
	}
}

func TestRatePickerExternalAnchorRefreshesOncePerInterval(t *testing.T) {
	source := &fakeRateSource{name: "quidax", price: 1371.41}
	anchor := &ZeroExUSDCCNGNSpotExternalAnchor{
		cfg:    config.USDCCNGNSpotExternalAnchorConfig{Provider: RatePickerProvider, Timeout: time.Second, MaxAge: 15 * time.Minute, MaxDeviationBPS: 100},
		picker: &ratePicker{sources: []rateSource{source}, timeout: time.Second, now: time.Now, health: map[string]*rateSourceHealth{}},
	}

	first := anchor.Fetch(context.Background())
	second := anchor.Fetch(context.Background())
	if !first.Present || first.Price != 1371.41 || !second.Present || second.Price != 1371.41 {
		t.Fatalf("first = %+v, second = %+v", first, second)
	}
	if second.RefreshAttempted {
		t.Fatal("second fetch inside the refresh interval should serve the cached price")
	}
	if got := source.calls.Load(); got != 1 {
		t.Fatalf("upstream queried %d times, want 1 per %s", got, ratePickerRefreshInterval)
	}
}

// A move larger than the fetch-to-fetch guard used to be rejected forever once the accepted price aged out:
// the expired price stayed the baseline. An expired price is no baseline, so the new one is accepted.
func TestExternalAnchorAcceptsAMoveOnceTheBaselineExpired(t *testing.T) {
	var hits atomic.Int32
	anchor := &ZeroExUSDCCNGNSpotExternalAnchor{
		cfg: config.USDCCNGNSpotExternalAnchorConfig{
			Provider: "0x", BaseURL: "https://example.invalid/price", ChainID: 8453, SellToken: "0xsell", BuyToken: "0xbuy", Amount: "1000000",
			Timeout: time.Second, MaxAge: 10 * time.Millisecond, MaxDeviationBPS: 100,
		},
		client: clientFunc(func(*http.Request) *http.Response {
			if hits.Add(1) == 1 {
				return jsonResponse(200, `{"price":"1326"}`)
			}
			return jsonResponse(200, `{"price":"1371"}`)
		}),
	}

	if first := anchor.Fetch(context.Background()); !first.Present || first.Price != 1326 {
		t.Fatalf("first = %+v", first)
	}
	time.Sleep(20 * time.Millisecond)
	if second := anchor.Fetch(context.Background()); !second.Present || second.Price != 1371 {
		t.Fatalf("second = %+v, want 1371 accepted once 1326 expired", second)
	}
}
