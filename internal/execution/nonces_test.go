package execution

import (
	"sync"
	"testing"
	"time"
)

// Ten bots allocating at once, all in the same microsecond, sign no nonce twice.
func TestNonceAllocatorNeverOverlapsAcrossBots(t *testing.T) {
	frozen := time.UnixMicro(1_793_580_000_000_000)
	a := &NonceAllocator{now: func() time.Time { return frozen }}
	const bots, rounds, width = 10, 50, 6 // 3 levels: 3 bids, 3 asks
	seen := map[uint64]int{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for b := 0; b < bots; b++ {
		wg.Add(1)
		go func(bot int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				base := a.Base(0, width)
				if base%2 != 0 {
					t.Errorf("odd base %d: bids must take the even offsets", base)
				}
				mu.Lock()
				for n := base; n < base+width; n++ {
					if prev, dup := seen[n]; dup {
						t.Errorf("nonce %d signed by bot %d and bot %d", n, prev, bot)
					}
					seen[n] = bot
				}
				mu.Unlock()
			}
		}(b)
	}
	wg.Wait()
	if len(seen) != bots*rounds*width {
		t.Fatalf("%d nonces, want %d", len(seen), bots*rounds*width)
	}
}

// A bot's persisted base, and the clock, still floor the allocation: a restart never goes back.
func TestNonceAllocatorRespectsTheFloorAndTheClock(t *testing.T) {
	clock := time.UnixMicro(1_000)
	a := &NonceAllocator{now: func() time.Time { return clock }}
	if got := a.Base(5_001, 4); got != 5_002 {
		t.Fatalf("floor 5001 -> %d, want 5002 (even, at or above the floor)", got)
	}
	clock = time.UnixMicro(1_000_000)
	if got := a.Base(0, 4); got != 2_000_000 {
		t.Fatalf("the clock floors it: %d", got)
	}
	if got := a.Base(0, 4); got != 2_000_004 {
		t.Fatalf("past the last range: %d", got)
	}
}
