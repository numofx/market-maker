package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
)

type Registry struct {
	mu                             sync.RWMutex
	openBidPresent                 float64
	openAskPresent                 float64
	inventory                      map[string]float64
	netInventory                   float64
	fillsBySide                    map[string]uint64
	cancelCategoryTotals           map[string]uint64
	quoteRefreshes                 uint64
	orderPlacements                uint64
	cancels                        uint64
	errors                         uint64
	partialFills                   uint64
	suppressedReplaces             uint64
	lastReference                  float64
	halted                         float64
	haltReason                     string
	anchorPrice                    float64
	externalAnchorPresent          float64
	externalAnchorAgeSeconds       float64
	externalAnchorPrice            float64
	externalAnchorRefreshes        uint64
	externalAnchorRefreshFailures  uint64
	referenceSource                string
	anchorLocalDeviationBPS        float64
	quoteAgeSeconds                float64
	lastKnownQuoteAge              float64
	exchangeQuoteAgeSeconds        float64
	lastMarketDataRefresh          float64
	lastBalanceRefresh             float64
	lastAnchorRefresh              float64
	exchangeMarketDataFreshnessAge float64
	balanceFreshnessAge            float64
	anchorFreshnessAge             float64
	cancelsPerMinute               float64
	cancelReplaceRatio             float64
	liveQuotedSpreadBPS            float64
	operatorMode                   string
	staleExchangeMarketData        float64
	staleBalances                  float64
	staleAnchorData                float64
	healthOK                       bool
	healthReason                   string
	readyOK                        bool
	readyReason                    string
}

func New() *Registry {
	return &Registry{
		inventory:            make(map[string]float64),
		fillsBySide:          make(map[string]uint64),
		cancelCategoryTotals: make(map[string]uint64),
		healthOK:             true,
		readyOK:              true,
	}
}

func (r *Registry) SetOpenBidPresent(present bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.openBidPresent = boolToFloat(present)
}

func (r *Registry) SetOpenAskPresent(present bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.openAskPresent = boolToFloat(present)
}

func (r *Registry) SetInventory(asset string, value float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inventory[asset] = value
}

func (r *Registry) SetNetInventory(value float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.netInventory = value
}

func (r *Registry) IncQuoteRefresh() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.quoteRefreshes++
}

func (r *Registry) IncPlacements() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.orderPlacements++
}

func (r *Registry) IncCancels() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancels++
}

func (r *Registry) IncCancelCategory(category string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancelCategoryTotals[category]++
}

func (r *Registry) IncErrors() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors++
}

func (r *Registry) IncFill(side string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fillsBySide[side]++
}

func (r *Registry) IncPartialFills() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.partialFills++
}

func (r *Registry) IncSuppressedReplaces() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.suppressedReplaces++
}

func (r *Registry) SetLastReferencePrice(value float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastReference = value
}

func (r *Registry) SetHaltState(halted bool, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.halted = boolToFloat(halted)
	r.haltReason = reason
}

func (r *Registry) SetAnchorPrice(value float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.anchorPrice = value
}

func (r *Registry) SetExternalAnchor(present bool, ageSeconds, price float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.externalAnchorPresent = boolToFloat(present)
	r.externalAnchorAgeSeconds = ageSeconds
	r.externalAnchorPrice = price
}

func (r *Registry) IncExternalAnchorRefresh(success bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if success {
		r.externalAnchorRefreshes++
		return
	}
	r.externalAnchorRefreshFailures++
}

func (r *Registry) SetReferenceSource(source string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.referenceSource = source
}

func (r *Registry) SetAnchorLocalDeviationBPS(value float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.anchorLocalDeviationBPS = value
}

func (r *Registry) SetQuoteAgeSeconds(value float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.quoteAgeSeconds = value
	r.lastKnownQuoteAge = value
}

func (r *Registry) SetExchangeQuoteAgeSeconds(value float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.exchangeQuoteAgeSeconds = value
}

func (r *Registry) SetLastMarketDataRefresh(ts float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastMarketDataRefresh = ts
}

func (r *Registry) SetLastBalanceRefresh(ts float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastBalanceRefresh = ts
}

func (r *Registry) SetLastAnchorRefresh(ts float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastAnchorRefresh = ts
}

func (r *Registry) SetFreshnessAges(exchangeMarketDataAge, balanceAge, anchorAge float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.exchangeMarketDataFreshnessAge = exchangeMarketDataAge
	r.balanceFreshnessAge = balanceAge
	r.anchorFreshnessAge = anchorAge
}

func (r *Registry) SetOperatorMode(mode string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.operatorMode = mode
}

func (r *Registry) SetCancelsPerMinute(value float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancelsPerMinute = value
}

func (r *Registry) SetCancelReplaceRatio(value float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancelReplaceRatio = value
}

func (r *Registry) SetLiveQuotedSpreadBPS(value float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.liveQuotedSpreadBPS = value
}

func (r *Registry) SetDependencyStale(stage string, stale bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value := boolToFloat(stale)
	switch stage {
	case "exchange_market_data":
		r.staleExchangeMarketData = value
	case "balances":
		r.staleBalances = value
	case "anchor_data":
		r.staleAnchorData = value
	}
}

func (r *Registry) SetHealth(ok bool, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.healthOK = ok
	r.healthReason = reason
}

func (r *Registry) SetReadiness(ok bool, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.readyOK = ok
	r.readyReason = reason
}

func (r *Registry) SnapshotCounters() (map[string]uint64, uint64, map[string]uint64) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	fills := make(map[string]uint64, len(r.fillsBySide))
	for k, v := range r.fillsBySide {
		fills[k] = v
	}
	cancels := make(map[string]uint64, len(r.cancelCategoryTotals))
	for k, v := range r.cancelCategoryTotals {
		cancels[k] = v
	}
	return fills, r.partialFills, cancels
}

func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(r.render()))
	})
}

func (r *Registry) HealthHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
}

func (r *Registry) ReadyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		r.mu.RLock()
		ok := r.readyOK
		reason := r.readyReason
		r.mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(fmt.Sprintf(`{"status":"not_ready","reason":%q}`, reason)))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	})
}

func (r *Registry) render() string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var b strings.Builder
	fmt.Fprintf(&b, "mm_bot_open_bid_present %v\n", r.openBidPresent)
	fmt.Fprintf(&b, "mm_bot_open_ask_present %v\n", r.openAskPresent)
	fmt.Fprintf(&b, "mm_bot_net_inventory %v\n", r.netInventory)
	fmt.Fprintf(&b, "mm_bot_quote_refresh_total %d\n", r.quoteRefreshes)
	fmt.Fprintf(&b, "mm_bot_order_placements_total %d\n", r.orderPlacements)
	fmt.Fprintf(&b, "mm_bot_order_cancels_total %d\n", r.cancels)
	fmt.Fprintf(&b, "mm_bot_errors_total %d\n", r.errors)
	fmt.Fprintf(&b, "mm_bot_partial_fills_total %d\n", r.partialFills)
	fmt.Fprintf(&b, "mm_bot_suppressed_replaces_total %d\n", r.suppressedReplaces)
	fmt.Fprintf(&b, "mm_bot_last_reference_price %v\n", r.lastReference)
	fmt.Fprintf(&b, "mm_bot_halted %v\n", r.halted)
	if r.haltReason != "" {
		fmt.Fprintf(&b, "mm_bot_halt_reason{reason=%q} 1\n", r.haltReason)
	}
	fmt.Fprintf(&b, "mm_bot_anchor_price %v\n", r.anchorPrice)
	fmt.Fprintf(&b, "mm_bot_external_anchor_present %v\n", r.externalAnchorPresent)
	fmt.Fprintf(&b, "mm_bot_external_anchor_age_seconds %v\n", r.externalAnchorAgeSeconds)
	fmt.Fprintf(&b, "mm_bot_external_anchor_price %v\n", r.externalAnchorPrice)
	fmt.Fprintf(&b, "mm_bot_external_anchor_refresh_total %d\n", r.externalAnchorRefreshes)
	fmt.Fprintf(&b, "mm_bot_external_anchor_refresh_failures_total %d\n", r.externalAnchorRefreshFailures)
	fmt.Fprintf(&b, "mm_bot_anchor_local_deviation_bps %v\n", r.anchorLocalDeviationBPS)
	fmt.Fprintf(&b, "mm_bot_quote_age_seconds %v\n", r.quoteAgeSeconds)
	fmt.Fprintf(&b, "mm_bot_last_known_quote_age_seconds %v\n", r.lastKnownQuoteAge)
	fmt.Fprintf(&b, "mm_bot_exchange_quote_age_seconds %v\n", r.exchangeQuoteAgeSeconds)
	fmt.Fprintf(&b, "mm_bot_last_market_data_refresh_unix %v\n", r.lastMarketDataRefresh)
	fmt.Fprintf(&b, "mm_bot_last_balance_refresh_unix %v\n", r.lastBalanceRefresh)
	fmt.Fprintf(&b, "mm_bot_last_anchor_refresh_unix %v\n", r.lastAnchorRefresh)
	fmt.Fprintf(&b, "mm_bot_exchange_market_data_freshness_age_seconds %v\n", r.exchangeMarketDataFreshnessAge)
	fmt.Fprintf(&b, "mm_bot_balance_freshness_age_seconds %v\n", r.balanceFreshnessAge)
	fmt.Fprintf(&b, "mm_bot_anchor_freshness_age_seconds %v\n", r.anchorFreshnessAge)
	fmt.Fprintf(&b, "mm_bot_cancels_per_minute %v\n", r.cancelsPerMinute)
	fmt.Fprintf(&b, "mm_bot_cancel_replace_ratio %v\n", r.cancelReplaceRatio)
	fmt.Fprintf(&b, "mm_bot_live_quoted_spread_bps %v\n", r.liveQuotedSpreadBPS)
	fmt.Fprintf(&b, "mm_bot_ready %v\n", boolToFloat(r.readyOK))
	fmt.Fprintf(&b, "mm_bot_dependency_stale{dependency=%q} %v\n", "exchange_market_data", r.staleExchangeMarketData)
	fmt.Fprintf(&b, "mm_bot_dependency_stale{dependency=%q} %v\n", "balances", r.staleBalances)
	fmt.Fprintf(&b, "mm_bot_dependency_stale{dependency=%q} %v\n", "anchor_data", r.staleAnchorData)
	if r.operatorMode != "" {
		fmt.Fprintf(&b, "mm_bot_operator_mode{mode=%q} 1\n", r.operatorMode)
	}
	for _, source := range []string{"book", "trade", "external", "none"} {
		fmt.Fprintf(&b, "mm_bot_reference_source{source=%q} %v\n", source, boolToFloat(r.referenceSource == source))
	}

	fillSides := make([]string, 0, len(r.fillsBySide))
	for side := range r.fillsBySide {
		fillSides = append(fillSides, side)
	}
	sort.Strings(fillSides)
	for _, side := range fillSides {
		fmt.Fprintf(&b, "mm_bot_fills_total{side=%q} %d\n", side, r.fillsBySide[side])
	}
	cancelCats := make([]string, 0, len(r.cancelCategoryTotals))
	for category := range r.cancelCategoryTotals {
		cancelCats = append(cancelCats, category)
	}
	sort.Strings(cancelCats)
	for _, category := range cancelCats {
		fmt.Fprintf(&b, "mm_bot_order_cancels_total_by_category{category=%q} %d\n", category, r.cancelCategoryTotals[category])
	}

	keys := make([]string, 0, len(r.inventory))
	for asset := range r.inventory {
		keys = append(keys, asset)
	}
	sort.Strings(keys)
	for _, asset := range keys {
		fmt.Fprintf(&b, "mm_bot_inventory{asset=%q} %v\n", asset, r.inventory[asset])
	}
	return b.String()
}

func boolToFloat(value bool) float64 {
	if value {
		return 1
	}
	return 0
}
