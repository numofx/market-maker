package execution

import (
	"sync"
	"time"
)

// NonceAllocator hands out order-nonce bases to several bots signing for one owner in one process (the options
// quoter: a bot per strike). The TradeModule keys signed orders by (owner, nonce), so two bots must never sign the same
// nonce; each bot's own base (its persisted NextNonceBase, or the clock) is not enough when two allocate in the same
// microsecond. The allocator returns a base at or above the bot's own and above every range it has handed out, so the
// ranges never overlap. A single bot (the spot, perp and futures makers) has none and allocates as it always has.
type NonceAllocator struct {
	mu   sync.Mutex
	next uint64
	now  func() time.Time
}

func NewNonceAllocator() *NonceAllocator {
	return &NonceAllocator{now: time.Now}
}

// Base returns an even base for `width` nonces (base .. base+width-1), at least `floor` (the bot's persisted
// NextNonceBase) and the clock's UnixMicro x 2, and past every range handed out before.
func (a *NonceAllocator) Base(floor uint64, width uint64) uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	base := floor
	if clock := uint64(a.now().UnixMicro()) * 2; clock > base {
		base = clock
	}
	if a.next > base {
		base = a.next
	}
	if base%2 != 0 {
		base++
	}
	a.next = base + width
	return base
}
