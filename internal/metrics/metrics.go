package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
)

type Registry struct {
	mu              sync.RWMutex
	openBidPresent  float64
	openAskPresent  float64
	inventory       map[string]float64
	quoteRefreshes  uint64
	orderPlacements uint64
	cancels         uint64
	errors          uint64
	lastReference   float64
}

func New() *Registry {
	return &Registry{
		inventory: make(map[string]float64),
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

func (r *Registry) IncErrors() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors++
}

func (r *Registry) SetLastReferencePrice(value float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastReference = value
}

func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(r.render()))
	})
}

func HealthHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
}

func (r *Registry) render() string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var b strings.Builder
	fmt.Fprintf(&b, "mm_bot_open_bid_present %v\n", r.openBidPresent)
	fmt.Fprintf(&b, "mm_bot_open_ask_present %v\n", r.openAskPresent)
	fmt.Fprintf(&b, "mm_bot_quote_refresh_total %d\n", r.quoteRefreshes)
	fmt.Fprintf(&b, "mm_bot_order_placements_total %d\n", r.orderPlacements)
	fmt.Fprintf(&b, "mm_bot_order_cancels_total %d\n", r.cancels)
	fmt.Fprintf(&b, "mm_bot_errors_total %d\n", r.errors)
	fmt.Fprintf(&b, "mm_bot_last_reference_price %v\n", r.lastReference)

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
