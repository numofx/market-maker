package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/numofx/market-maker/internal/exchange"
)

// ReferenceTradeMaxAge bounds how old the most recent trade may be to still stand in for a live
// reference price. Older than this, an empty book falls through to the oracle rather than anchoring
// the mid on a stale print — a days-old trade must not read as the current price, which would trip
// the anchor-deviation guard against a rate that has since moved and halt the bot.
const ReferenceTradeMaxAge = 5 * time.Minute

// FreshTradePrice returns the most recent trade's price when it is recent enough (per
// ReferenceTradeMaxAge, measured against the snapshot's own market-data timestamp) to serve as a
// local reference. It returns ok=false for an empty or stale trade, or when the snapshot has no
// market-data timestamp to age against.
func FreshTradePrice(snapshot Snapshot) (float64, bool) {
	if len(snapshot.RecentTrades) == 0 {
		return 0, false
	}
	trade := snapshot.RecentTrades[0]
	if trade.Price <= 0 || snapshot.LastMarketDataRefresh.IsZero() {
		return 0, false
	}
	if snapshot.LastMarketDataRefresh.Sub(trade.CreatedAt) > ReferenceTradeMaxAge {
		return 0, false
	}
	return trade.Price, true
}

// ReferenceTradePrice is the trade price a local reference may stand on. On USDCcNGN-SPOT the venue's
// own book is the price, so its last trade stands however old it is: the oracle no longer gates spot
// (see marketdata.Loader.Load), and an old print was only ever dangerous because an oracle guard
// compared against it. Other markets keep the ReferenceTradeMaxAge cutoff, since their anchor still
// feeds the deviation guard.
func ReferenceTradePrice(snapshot Snapshot) (float64, bool) {
	if snapshot.Market != "USDCcNGN-SPOT" {
		return FreshTradePrice(snapshot)
	}
	if len(snapshot.RecentTrades) == 0 || snapshot.RecentTrades[0].Price <= 0 {
		return 0, false
	}
	return snapshot.RecentTrades[0].Price, true
}

type AssetPosition struct {
	Total     float64
	Reserved  float64
	Available float64
	// Reusable is the part of Reserved this bot frees again each cycle -- its own replaceable
	// orders on this market. The quoting budget is Available + Reusable, and both come from the
	// client so the reservation is computed exactly once. See exchange.Balance.Reusable.
	Reusable float64
}

type Snapshot struct {
	Market string
	// BestBid and BestAsk are other participants' best prices: the bot's own resting orders are
	// excluded, since the reference must not be priced off the bot's own quotes.
	BestBid                        float64
	BestAsk                        float64
	ReferencePrice                 float64
	ReferenceSource                string
	LocalReferencePrice            float64
	LocalReferenceSource           string
	AnchorPrice                    float64
	AnchorSource                   string
	AnchorDeviationBPS             float64
	ExternalAnchorPrice            float64
	LastExternalAnchorRefresh      time.Time
	ExternalAnchorRefreshAttempted bool
	ExternalAnchorRefreshFailed    bool
	InventoryByAsset               map[string]float64
	Positions                      map[string]AssetPosition
	OpenOrders                     []exchange.Order
	RecentTrades                   []exchange.Trade
	LastQuoteUpdate                time.Time
	LastMarketDataRefresh          time.Time
	LastBalanceRefresh             time.Time
	LastAnchorRefresh              time.Time
	LocalQuoteAge                  time.Duration
	ExchangeQuoteAge               time.Duration
}

func (s Snapshot) Inventory(asset string) float64 {
	return s.InventoryByAsset[asset]
}

func (s Snapshot) Position(asset string) AssetPosition {
	return s.Positions[asset]
}

// ControlState is the operator's kill/pause as last set through the control API.
//
// Persisted so a restart comes back KILLED or PAUSED rather than quoting: a crash-looping task that
// forgot it had been killed would re-place a full ladder on every boot, which is exactly when a kill
// is being relied on. MM_STATE_FILE lives under /tmp on Fargate, which does not survive a task
// replacement -- so this covers in-task restarts only, and the terminal re-asserts kill on connect
// for the rest.
type ControlState struct {
	Killed      bool      `json:"killed"`
	KillReason  string    `json:"kill_reason,omitempty"`
	KilledAt    time.Time `json:"killed_at,omitempty"`
	Paused      bool      `json:"paused"`
	PauseReason string    `json:"pause_reason,omitempty"`
	PausedAt    time.Time `json:"paused_at,omitempty"`
}

type Persistent struct {
	NextNonceBase         uint64             `json:"next_nonce_base"`
	LastNonceBySide       map[string]uint64  `json:"last_nonce_by_side"`
	LastSubmittedBidOrder string             `json:"last_submitted_bid_order_id,omitempty"`
	LastSubmittedAskOrder string             `json:"last_submitted_ask_order_id,omitempty"`
	LastAdoptedBidOrder   string             `json:"last_adopted_bid_order_id,omitempty"`
	LastAdoptedAskOrder   string             `json:"last_adopted_ask_order_id,omitempty"`
	LastHaltReason        string             `json:"last_halt_reason,omitempty"`
	LastInventorySnapshot map[string]float64 `json:"last_inventory_snapshot,omitempty"`
	Control               *ControlState      `json:"control,omitempty"`
}

// Store reads and writes MM_STATE_FILE.
//
// Two writers share it: the quoting loop saves nonces every cycle, and the control API saves a kill
// the moment it arrives, from an HTTP goroutine. Unserialized, their writes could interleave into a
// torn file, and a whole-struct save from the loop would overwrite a kill written a moment earlier.
// So every write goes through one lock, and the overlay -- the controller's current kill/pause --
// is applied under that lock at write time, whichever writer is holding it.
type Store struct {
	path    string
	mu      sync.Mutex
	overlay func(*Persistent)
}

func NewStore(path string) *Store {
	return &Store{path: path}
}

// SetOverlay installs a hook applied to every value just before it is written. The hook must not
// call back into the Store.
func (s *Store) SetOverlay(fn func(*Persistent)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.overlay = fn
}

func (s *Store) Load() (Persistent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *Store) loadLocked() (Persistent, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Persistent{LastNonceBySide: map[string]uint64{}}, nil
		}
		return Persistent{}, err
	}
	var out Persistent
	if err := json.Unmarshal(raw, &out); err != nil {
		return Persistent{}, err
	}
	if out.LastNonceBySide == nil {
		out.LastNonceBySide = map[string]uint64{}
	}
	if out.LastInventorySnapshot == nil {
		out.LastInventorySnapshot = map[string]float64{}
	}
	return out, nil
}

func (s *Store) Save(value Persistent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(value)
}

// Update rewrites the file from its current contents, so a writer that owns only part of the state
// (the controller) does not clobber nonce progression it never read.
func (s *Store) Update(fn func(*Persistent)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, err := s.loadLocked()
	if err != nil {
		return err
	}
	if fn != nil {
		fn(&value)
	}
	return s.saveLocked(value)
}

func (s *Store) saveLocked(value Persistent) error {
	if s.overlay != nil {
		s.overlay(&value)
	}
	if value.LastNonceBySide == nil {
		value.LastNonceBySide = map[string]uint64{}
	}
	if value.LastInventorySnapshot == nil {
		value.LastInventorySnapshot = map[string]float64{}
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, raw, 0o644)
}
