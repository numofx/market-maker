package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/numofx/market-maker/internal/exchange"
)

func TestStoreBackwardCompatibleLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"next_nonce_base":42,"last_nonce_by_side":{"buy":40}}`), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	got, err := NewStore(path).Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.NextNonceBase != 42 {
		t.Fatalf("NextNonceBase = %d want 42", got.NextNonceBase)
	}
	if got.LastInventorySnapshot == nil {
		t.Fatal("LastInventorySnapshot should be initialized")
	}
}

func TestStorePersistsOperationalFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewStore(path)
	want := Persistent{
		NextNonceBase:         100,
		LastNonceBySide:       map[string]uint64{"buy": 100},
		LastSubmittedBidOrder: "bid-1",
		LastSubmittedAskOrder: "ask-1",
		LastAdoptedBidOrder:   "bid-0",
		LastAdoptedAskOrder:   "ask-0",
		LastHaltReason:        "kill switch active",
		LastInventorySnapshot: map[string]float64{"USDC": 12.5},
	}
	if err := store.Save(want); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.LastSubmittedBidOrder != want.LastSubmittedBidOrder || got.LastHaltReason != want.LastHaltReason {
		t.Fatalf("loaded persistent = %#v", got)
	}
	if got.LastInventorySnapshot["USDC"] != 12.5 {
		t.Fatalf("inventory snapshot = %#v", got.LastInventorySnapshot)
	}
}

// A state file written before the control API existed has no control block, and must load as
// "not killed, not paused" rather than failing.
func TestStoreLoadsWithoutControlState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"next_nonce_base":42,"last_halt_reason":"kill switch active"}`), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	got, err := NewStore(path).Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.Control != nil {
		t.Fatalf("Control = %#v, want nil", got.Control)
	}
}

// The quoting loop saves a whole Persistent every cycle, built from a copy it read before the kill
// arrived. The overlay is what stops that save from writing the kill back out of the file.
func TestStoreOverlayWinsOverAWholeStructSave(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "state.json"))
	store.SetOverlay(func(p *Persistent) {
		p.Control = &ControlState{Killed: true, KillReason: "drill"}
	})
	if err := store.Save(Persistent{NextNonceBase: 7}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.NextNonceBase != 7 || got.Control == nil || !got.Control.Killed || got.Control.KillReason != "drill" {
		t.Fatalf("loaded %#v, want nonce 7 and the kill preserved", got)
	}
}

// The controller owns only the control block. Update must rewrite from the file, so a kill does not
// reset nonce progression the controller never read -- a reset nonce base is owner+nonce reuse.
func TestStoreUpdatePreservesNonceProgression(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err := store.Save(Persistent{NextNonceBase: 42, LastNonceBySide: map[string]uint64{"buy": 40}}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.Update(func(p *Persistent) { p.Control = &ControlState{Paused: true} }); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.NextNonceBase != 42 || got.LastNonceBySide["buy"] != 40 {
		t.Fatalf("nonce progression lost: %#v", got)
	}
	if got.Control == nil || !got.Control.Paused {
		t.Fatalf("Control = %#v, want paused", got.Control)
	}
}

func TestFreshTradePrice(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	base := Snapshot{LastMarketDataRefresh: now}

	// Fresh trade counts.
	s := base
	s.RecentTrades = []exchange.Trade{{Price: 1550, CreatedAt: now.Add(-time.Minute)}}
	if p, ok := FreshTradePrice(s); !ok || p != 1550 {
		t.Fatalf("fresh trade: got (%v,%v) want (1550,true)", p, ok)
	}

	// Stale trade (older than the cutoff) is rejected.
	s.RecentTrades = []exchange.Trade{{Price: 1550, CreatedAt: now.Add(-ReferenceTradeMaxAge - time.Second)}}
	if _, ok := FreshTradePrice(s); ok {
		t.Fatal("stale trade should not be a reference")
	}

	// No market-data timestamp to age against → rejected.
	s2 := Snapshot{RecentTrades: []exchange.Trade{{Price: 1550, CreatedAt: now}}}
	if _, ok := FreshTradePrice(s2); ok {
		t.Fatal("missing market-data timestamp should reject the trade")
	}
}

func TestReferenceTradePriceOnSpotIgnoresAge(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	old := []exchange.Trade{{Price: 1327.34, CreatedAt: now.Add(-24 * time.Hour)}}

	spot := Snapshot{Market: "USDCcNGN-SPOT", LastMarketDataRefresh: now, RecentTrades: old}
	if p, ok := ReferenceTradePrice(spot); !ok || p != 1327.34 {
		t.Fatalf("spot: got (%v,%v) want (1327.34,true)", p, ok)
	}

	// Other markets still refuse a stale print: their anchor feeds the deviation guard.
	future := Snapshot{Market: "USDCcNGN-SEP16-2026", LastMarketDataRefresh: now, RecentTrades: old}
	if _, ok := ReferenceTradePrice(future); ok {
		t.Fatal("non-spot stale trade should not be a reference")
	}

	if _, ok := ReferenceTradePrice(Snapshot{Market: "USDCcNGN-SPOT"}); ok {
		t.Fatal("spot with no trades has no trade reference")
	}
}
