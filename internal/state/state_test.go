package state

import (
	"os"
	"path/filepath"
	"testing"
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
