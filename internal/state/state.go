package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/numofx/market-maker/internal/exchange"
)

type AssetPosition struct {
	Total     float64
	Reserved  float64
	Available float64
}

type Snapshot struct {
	Market                string
	BestBid               float64
	BestAsk               float64
	ReferencePrice        float64
	LocalReferencePrice   float64
	AnchorPrice           float64
	AnchorSource          string
	AnchorDeviationBPS    float64
	InventoryByAsset      map[string]float64
	Positions             map[string]AssetPosition
	OpenOrders            []exchange.Order
	RecentTrades          []exchange.Trade
	LastQuoteUpdate       time.Time
	LastMarketDataRefresh time.Time
	LastBalanceRefresh    time.Time
	LastAnchorRefresh     time.Time
	LocalQuoteAge         time.Duration
	ExchangeQuoteAge      time.Duration
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
