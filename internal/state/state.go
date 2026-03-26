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
	Market           string
	BestBid          float64
	BestAsk          float64
	ReferencePrice   float64
	InventoryByAsset map[string]float64
	Positions        map[string]AssetPosition
	OpenOrders       []exchange.Order
	RecentTrades     []exchange.Trade
	LastQuoteUpdate  time.Time
}

func (s Snapshot) Inventory(asset string) float64 {
	return s.InventoryByAsset[asset]
}

func (s Snapshot) Position(asset string) AssetPosition {
	return s.Positions[asset]
}

type Persistent struct {
	NextNonceBase   uint64            `json:"next_nonce_base"`
	LastNonceBySide map[string]uint64 `json:"last_nonce_by_side"`
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
	return out, nil
}

func (s *Store) Save(value Persistent) error {
	if value.LastNonceBySide == nil {
		value.LastNonceBySide = map[string]uint64{}
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
