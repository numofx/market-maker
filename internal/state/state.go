package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

type AssetPosition struct {
	Total     float64
	Reserved  float64
	Available float64
}

type Snapshot struct {
	Market                         string
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

type Persistent struct {
	NextNonceBase         uint64             `json:"next_nonce_base"`
	LastNonceBySide       map[string]uint64  `json:"last_nonce_by_side"`
	LastSubmittedBidOrder string             `json:"last_submitted_bid_order_id,omitempty"`
	LastSubmittedAskOrder string             `json:"last_submitted_ask_order_id,omitempty"`
	LastAdoptedBidOrder   string             `json:"last_adopted_bid_order_id,omitempty"`
	LastAdoptedAskOrder   string             `json:"last_adopted_ask_order_id,omitempty"`
	LastHaltReason        string             `json:"last_halt_reason,omitempty"`
	LastInventorySnapshot map[string]float64 `json:"last_inventory_snapshot,omitempty"`
}

type Store struct {
	path string
}

func NewStore(path string) *Store {
	return &Store{path: path}
}

func (s *Store) Load() (Persistent, error) {
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
