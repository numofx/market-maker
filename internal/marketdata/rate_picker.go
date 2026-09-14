package marketdata

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RatePickerProvider is the MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_PROVIDER value for spot's fallback price: a
// Go port of github.com/wrappedcbdc/cngn-rate-picker (v0.2.2, 0d1d1b5d). It quotes NGN per USDT from the
// library's keyless providers in its priority order -- Quidax, Textile, Bybit P2P -- with the library's
// default threshold of 1: every source is queried, and the highest-priority success is the price.
//
// Blockradar is left out because it needs an API key. Two deliberate departures from the library, both
// found against the live feeds on 2026-09-14:
//   - Quidax reads the usdtngn market, not the library's default usdtcngn. usdtcngn had zero volume for
//     the hour while its candles kept repeating the last close, and the library's staleness guard, which
//     ages the newest candle whatever its volume, accepted it.
//   - For the same reason Quidax averages and ages only candles that actually traded.
//
// USDT rather than USDC because USDT/NGN is the liquid corridor on every one of these venues (Quidax lists
// no cNGN/USDC market with trades, and Textile's USDC_NGN book had no bid); the two stablecoins quoted
// within a naira of each other against NGN on the same day.
const RatePickerProvider = "cngn-rate-picker"

// ratePickerRefreshInterval bounds how often the upstream feeds are read. Each source is a 1-hour TWAP or a
// median of standing ads, so a minute-old read is fresh, while the quote loop runs every couple of seconds.
const ratePickerRefreshInterval = time.Minute

const (
	ratePickerBreakerFailures = 3
	ratePickerBreakerCooldown = 30 * time.Second
)

var errRateSourceCircuitOpen = errors.New("skipped (circuit open)")

// rateSource is one upstream venue, normalised to NGN per 1 USDT.
type rateSource interface {
	Name() string
	PriceInNGN(ctx context.Context, now time.Time) (float64, error)
}

type rateSourceResult struct {
	Provider string
	Price    float64
	Err      error
}

type rateSourceHealth struct {
	failures int
	openedAt time.Time
}

// ratePicker is the library's ExchangeRatePicker in parallel mode with threshold 1, plus its circuit breaker:
// a source that fails three times in a row is skipped for 30 seconds.
type ratePicker struct {
	sources []rateSource
	timeout time.Duration
	now     func() time.Time

	mu     sync.Mutex
	health map[string]*rateSourceHealth
}

func newRatePicker(client *http.Client, timeout time.Duration) *ratePicker {
	return &ratePicker{
		sources: []rateSource{
			&quidaxRateSource{
				client:       client,
				baseURL:      "https://openapi.quidax.io/exchange-open-api/api/v1",
				market:       "usdtngn",
				window:       time.Hour,
				maxStaleness: 6 * time.Hour,
			},
			&textileRateSource{
				client:   client,
				baseURL:  "https://api.textilecredit.com",
				tickerID: "USDT_NGN",
				window:   time.Hour,
				limit:    200,
			},
			&bybitP2PRateSource{
				client:                 client,
				baseURL:                "https://api2.bybit.com",
				asset:                  "USDT",
				pageSize:               50,
				minCompletedOrders:     100,
				minCompletionRate:      0.9,
				maxAvgReleaseSeconds:   900,
				maxDeviationFromMedian: 0.02,
			},
		},
		timeout: timeout,
		now:     time.Now,
		health:  map[string]*rateSourceHealth{},
	}
}

// Pick queries every source whose circuit is closed, concurrently, so a refresh stalls the quote loop for at
// most one timeout rather than the sum of them. It returns the first success in priority order and every
// source's outcome.
func (p *ratePicker) Pick(ctx context.Context) (rateSourceResult, []rateSourceResult, error) {
	now := p.now()
	results := make([]rateSourceResult, len(p.sources))
	var wg sync.WaitGroup
	for i, source := range p.sources {
		results[i].Provider = source.Name()
		if p.circuitOpen(source.Name(), now) {
			results[i].Err = errRateSourceCircuitOpen
			continue
		}
		wg.Add(1)
		go func(i int, source rateSource) {
			defer wg.Done()
			sourceCtx := ctx
			if p.timeout > 0 {
				var cancel context.CancelFunc
				sourceCtx, cancel = context.WithTimeout(ctx, p.timeout)
				defer cancel()
			}
			price, err := source.PriceInNGN(sourceCtx, now)
			if err == nil && (price <= 0 || math.IsNaN(price) || math.IsInf(price, 0)) {
				err = fmt.Errorf("invalid price %v", price)
			}
			results[i].Price, results[i].Err = price, err
		}(i, source)
	}
	wg.Wait()

	var failures []string
	used := -1
	for i, result := range results {
		if !errors.Is(result.Err, errRateSourceCircuitOpen) {
			p.record(result.Provider, result.Err == nil, now)
		}
		if result.Err != nil {
			failures = append(failures, result.Provider+": "+result.Err.Error())
			continue
		}
		if used < 0 {
			used = i
		}
	}
	if used < 0 {
		return rateSourceResult{}, results, fmt.Errorf("every rate source failed: %s", strings.Join(failures, "; "))
	}
	return results[used], results, nil
}

func (p *ratePicker) circuitOpen(name string, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	h := p.health[name]
	if h == nil || h.openedAt.IsZero() {
		return false
	}
	if now.Sub(h.openedAt) >= ratePickerBreakerCooldown {
		// Cooldown elapsed: half-open, allow one trial call.
		h.openedAt = time.Time{}
		h.failures = 0
		return false
	}
	return true
}

func (p *ratePicker) record(name string, ok bool, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h := p.health[name]
	if h == nil {
		h = &rateSourceHealth{}
		p.health[name] = h
	}
	if ok {
		h.failures = 0
		h.openedAt = time.Time{}
		return
	}
	h.failures++
	if h.failures >= ratePickerBreakerFailures {
		h.openedAt = now
	}
}

// fetchRatePicker serves the rate picker through the spot external-anchor contract.
func (s *ZeroExUSDCCNGNSpotExternalAnchor) fetchRatePicker(ctx context.Context) (ExternalAnchorQuote, error) {
	s.mu.Lock()
	if s.picker == nil {
		s.picker = newRatePicker(s.client, s.cfg.Timeout)
	}
	picker := s.picker
	s.mu.Unlock()

	used, all, err := picker.Pick(ctx)
	if err != nil {
		return ExternalAnchorQuote{}, err
	}
	attrs := []any{"market", "USDCcNGN-SPOT", "provider", used.Provider, "price", used.Price}
	for _, result := range all {
		if result.Err != nil {
			attrs = append(attrs, result.Provider, "error: "+result.Err.Error())
		} else {
			attrs = append(attrs, result.Provider, result.Price)
		}
	}
	slog.Info("rate picker quote", attrs...)
	return ExternalAnchorQuote{
		Price:            used.Price,
		Present:          true,
		FetchedAt:        time.Now().UTC(),
		RefreshAttempted: true,
	}, nil
}

// quidaxRateSource is the library's QuidaxProvider: a time-weighted average of 1-minute candle closes.
type quidaxRateSource struct {
	client       *http.Client
	baseURL      string
	market       string
	window       time.Duration
	maxStaleness time.Duration
}

func (s *quidaxRateSource) Name() string { return "quidax" }

func (s *quidaxRateSource) PriceInNGN(ctx context.Context, now time.Time) (float64, error) {
	// Enough 1-minute candles to reach back to the staleness limit, so a market that last traded before the
	// TWAP window can still be aged. The endpoint caps a request at 1000.
	limit := int(s.maxStaleness/time.Minute) + 1
	if limit > 1000 {
		limit = 1000
	}
	var body struct {
		Message string  `json:"message"`
		Data    [][]any `json:"data"`
	}
	endpoint := fmt.Sprintf("%s/markets/%s/k?period=1&limit=%d", s.baseURL, url.PathEscape(s.market), limit)
	if err := doJSON(ctx, s.client, http.MethodGet, endpoint, nil, &body); err != nil {
		return 0, err
	}

	// A candle with no volume repeats the previous close; it is not a price anyone traded at.
	var traded []pricePoint
	for _, candle := range body.Data {
		if len(candle) < 6 {
			continue
		}
		at, okAt := numberOf(candle[0])
		closePrice, okClose := numberOf(candle[4])
		volume, okVolume := numberOf(candle[5])
		if !okAt || !okClose || !okVolume || closePrice <= 0 || volume <= 0 {
			continue
		}
		traded = append(traded, pricePoint{price: closePrice, at: time.UnixMilli(int64(at))})
	}
	if len(traded) == 0 {
		return 0, fmt.Errorf("quidax market %q has no traded candles in the last %s", s.market, s.maxStaleness)
	}
	newest := newestPricePoint(traded)
	if age := now.Sub(newest.at); age > s.maxStaleness {
		return 0, fmt.Errorf("quidax market %q last traded %s ago", s.market, age.Round(time.Minute))
	}
	windowed := pricePointsSince(traded, now.Add(-s.window))
	if len(windowed) == 0 {
		// Fresh, but nothing traded inside the window: the newest traded close is still the price that stood.
		windowed = []pricePoint{newest}
	}
	return timeWeightedAverage(windowed, now), nil
}

// textileRateSource is the library's TextileProvider: a time-weighted average of cleared trades, falling back
// to the order book when nothing cleared in the window.
type textileRateSource struct {
	client   *http.Client
	baseURL  string
	tickerID string
	window   time.Duration
	limit    int
}

func (s *textileRateSource) Name() string { return "textile" }

func (s *textileRateSource) PriceInNGN(ctx context.Context, now time.Time) (float64, error) {
	type textileTrade struct {
		Price          any `json:"price"`
		TradeTimestamp any `json:"trade_timestamp"`
	}
	var trades struct {
		Buy   []textileTrade `json:"buy"`
		Sell  []textileTrade `json:"sell"`
		Error string         `json:"error"`
	}
	tradesURL := fmt.Sprintf("%s/historical_trades?ticker_id=%s&limit=%d&start_time=%d",
		s.baseURL, url.QueryEscape(s.tickerID), s.limit, now.Add(-s.window).Unix())
	if err := doJSON(ctx, s.client, http.MethodGet, tradesURL, nil, &trades); err != nil {
		return 0, err
	}
	if trades.Buy == nil && trades.Sell == nil {
		return 0, fmt.Errorf("unexpected textile trades response for %q: %s", s.tickerID, trades.Error)
	}
	var points []pricePoint
	for _, trade := range append(trades.Buy, trades.Sell...) {
		price, okPrice := numberOf(trade.Price)
		at, okAt := numberOf(trade.TradeTimestamp)
		if okPrice && okAt && price > 0 {
			points = append(points, pricePoint{price: price, at: time.Unix(int64(at), 0)})
		}
	}
	if len(points) > 0 {
		return timeWeightedAverage(points, now), nil
	}

	var tickers []struct {
		TickerID  string `json:"ticker_id"`
		Bid       any    `json:"bid"`
		Ask       any    `json:"ask"`
		LastPrice any    `json:"last_price"`
	}
	tickerURL := fmt.Sprintf("%s/tickers?ticker_id=%s", s.baseURL, url.QueryEscape(s.tickerID))
	if err := doJSON(ctx, s.client, http.MethodGet, tickerURL, nil, &tickers); err != nil {
		return 0, err
	}
	if len(tickers) == 0 {
		return 0, fmt.Errorf("textile returned no ticker for %q", s.tickerID)
	}
	ticker := tickers[0]
	for _, candidate := range tickers {
		if strings.EqualFold(candidate.TickerID, s.tickerID) {
			ticker = candidate
			break
		}
	}
	// The library's mid: both sides when both are quoted, else the last cleared price.
	bid, okBid := numberOf(ticker.Bid)
	ask, okAsk := numberOf(ticker.Ask)
	if okBid && okAsk && bid > 0 && ask > 0 {
		return (bid + ask) / 2, nil
	}
	if last, ok := numberOf(ticker.LastPrice); ok && last > 0 {
		return last, nil
	}
	return 0, fmt.Errorf("textile ticker %q has no bid/ask or last price", s.tickerID)
}

// bybitP2PRateSource is the library's BybitP2PProvider on its keyless transport: the mid of the modal buy-ad
// and sell-ad prices among reputable, online advertisers.
type bybitP2PRateSource struct {
	client                 *http.Client
	baseURL                string
	asset                  string
	pageSize               int
	minCompletedOrders     float64
	minCompletionRate      float64
	maxAvgReleaseSeconds   float64
	maxDeviationFromMedian float64
}

func (s *bybitP2PRateSource) Name() string { return "bybit-p2p" }

func (s *bybitP2PRateSource) PriceInNGN(ctx context.Context, _ time.Time) (float64, error) {
	// On the web endpoint side "1" lists buy ads (the ask) and "0" sell ads (the bid).
	ask, err := s.sidePrice(ctx, "1", "buy")
	if err != nil {
		return 0, err
	}
	bid, err := s.sidePrice(ctx, "0", "sell")
	if err != nil {
		return 0, err
	}
	return (bid + ask) / 2, nil
}

const bybitP2PUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"

type bybitP2PAd struct {
	price          float64
	completed      float64
	completionRate float64
	avgRelease     float64
	hasAvgRelease  bool
	online         bool
}

func (s *bybitP2PRateSource) sidePrice(ctx context.Context, side, label string) (float64, error) {
	payload, err := json.Marshal(map[string]any{
		"tokenId":    s.asset,
		"currencyId": "NGN",
		"side":       side,
		"size":       strconv.Itoa(s.pageSize),
		"page":       "1",
		"payment":    []string{},
		"amount":     "",
	})
	if err != nil {
		return 0, err
	}
	var body struct {
		RetCode      *int   `json:"ret_code"`
		RetCodeCamel *int   `json:"retCode"`
		RetMsg       string `json:"ret_msg"`
		Result       struct {
			Items []struct {
				Price             any  `json:"price"`
				RecentOrderNum    any  `json:"recentOrderNum"`
				RecentExecuteRate any  `json:"recentExecuteRate"`
				AvgReleaseTime    any  `json:"avgReleaseTime"`
				IsOnline          bool `json:"isOnline"`
			} `json:"items"`
		} `json:"result"`
	}
	// The web endpoint answers browsers only: with Go's or curl's User-Agent it holds the connection open
	// until the client gives up (measured 2026-09-14; the same request with a browser UA returned in 0.4s).
	headers := map[string]string{"User-Agent": bybitP2PUserAgent}
	if err := doJSON(ctx, s.client, http.MethodPost, s.baseURL+"/fiat/otc/item/online", payload, &body, headers); err != nil {
		return 0, err
	}
	code := body.RetCode
	if code == nil {
		code = body.RetCodeCamel
	}
	if code != nil && *code != 0 {
		return 0, fmt.Errorf("bybit p2p returned %d: %s (%s)", *code, body.RetMsg, label)
	}

	ads := make([]bybitP2PAd, 0, len(body.Result.Items))
	for _, item := range body.Result.Items {
		price, ok := numberOf(item.Price)
		if !ok || price <= 0 {
			continue
		}
		completed, _ := numberOf(item.RecentOrderNum)
		rate, _ := numberOf(item.RecentExecuteRate)
		if rate > 1 {
			// Reported as a percentage on some responses and a fraction on others.
			rate /= 100
		}
		release, hasRelease := numberOf(item.AvgReleaseTime)
		ads = append(ads, bybitP2PAd{price: price, completed: completed, completionRate: rate, avgRelease: release, hasAvgRelease: hasRelease, online: item.IsOnline})
	}
	return s.modalPrice(ads, label)
}

func (s *bybitP2PRateSource) modalPrice(ads []bybitP2PAd, label string) (float64, error) {
	var reputable []bybitP2PAd
	for _, ad := range ads {
		// An ad without a release time is not held to that criterion, as in the library.
		if ad.completed >= s.minCompletedOrders && ad.completionRate >= s.minCompletionRate &&
			(!ad.hasAvgRelease || ad.avgRelease <= s.maxAvgReleaseSeconds) && ad.online {
			reputable = append(reputable, ad)
		}
	}
	if len(reputable) < 3 {
		return 0, fmt.Errorf("fewer than 3 reputable bybit p2p %s ads after fraud filtering (%d)", label, len(reputable))
	}

	prices := make([]float64, len(reputable))
	for i, ad := range reputable {
		prices[i] = ad.price
	}
	sort.Float64s(prices)
	median := prices[len(prices)/2]
	var filtered []bybitP2PAd
	for _, ad := range reputable {
		if math.Abs(ad.price-median)/median <= s.maxDeviationFromMedian {
			filtered = append(filtered, ad)
		}
	}
	if len(filtered) < 2 {
		filtered = reputable
	}

	// Mode of whole-naira prices; a tie goes to the price listed first.
	counts := map[float64]int{}
	var order []float64
	for _, ad := range filtered {
		naira := math.Round(ad.price)
		if counts[naira] == 0 {
			order = append(order, naira)
		}
		counts[naira]++
	}
	mode, best := 0.0, 0
	for _, naira := range order {
		if counts[naira] > best {
			mode, best = naira, counts[naira]
		}
	}
	return mode, nil
}

type pricePoint struct {
	price float64
	at    time.Time
}

// timeWeightedAverage is the library's timeWeightedAverage: each point is weighted by how long it stood as the
// most recent observation, the newest up to now, and points sharing a timestamp clamp to 1ms so each counts.
func timeWeightedAverage(points []pricePoint, now time.Time) float64 {
	sorted := append([]pricePoint(nil), points...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].at.Before(sorted[j].at) })
	if len(sorted) == 1 {
		return sorted[0].price
	}
	var weightedSum, totalWeight float64
	for i, point := range sorted {
		until := now
		if i+1 < len(sorted) {
			until = sorted[i+1].at
		}
		weight := until.Sub(point.at)
		if weight < time.Millisecond {
			weight = time.Millisecond
		}
		weightedSum += point.price * weight.Seconds()
		totalWeight += weight.Seconds()
	}
	return weightedSum / totalWeight
}

func pricePointsSince(points []pricePoint, from time.Time) []pricePoint {
	var out []pricePoint
	for _, point := range points {
		if !point.at.Before(from) {
			out = append(out, point)
		}
	}
	return out
}

func newestPricePoint(points []pricePoint) pricePoint {
	newest := points[0]
	for _, point := range points[1:] {
		if point.at.After(newest.at) {
			newest = point
		}
	}
	return newest
}

// numberOf reads a JSON number or numeric string; the upstream feeds use both.
func numberOf(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, !math.IsNaN(v) && !math.IsInf(v, 0)
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return parsed, err == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0)
	default:
		return 0, false
	}
}

func doJSON(ctx context.Context, client *http.Client, method, endpoint string, payload []byte, out any, headers ...map[string]string) error {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	for _, set := range headers {
		for key, value := range set {
			req.Header.Set(key, value)
		}
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return &ExternalAnchorFetchError{
			StatusCode:  resp.StatusCode,
			BodyPreview: truncateBody(string(raw)),
			Err:         fmt.Errorf("%s returned %d", req.URL.Host, resp.StatusCode),
		}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode %s response: %w", req.URL.Host, err)
	}
	return nil
}
