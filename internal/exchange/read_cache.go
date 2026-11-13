package exchange

import (
	"context"
	"encoding/hex"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// ReadCache shares chain reads between the exchange clients of one process. In options mode every strike is its own
// client on one subaccount, and each cycle all of them read the same latest header, the same forward, the same
// getAccountBalances and the same getMargin: ten strikes made ten copies of each, every two seconds, which is what
// exhausted the RPC plan's monthly capacity on 2026-11-12 and emptied every book on the venue.
//
// Identical reads are made once: a read already in flight is joined, and a finished one is reused while it is fresh.
// A read at "latest" is fresh for latestTTL, measured from when it was sent, so it must stay under the poll interval
// for each cycle to see new state. A read at an explicit block never changes and is kept for pinnedReadTTL. Errors are
// never reused, though the callers waiting on a read share its error.
type ReadCache struct {
	latestTTL time.Duration
	now       func() time.Time

	mu      sync.Mutex
	entries map[string]*readEntry
}

type readEntry struct {
	done   chan struct{}
	sent   time.Time
	pinned bool
	value  any
	err    error
}

// pinnedReadTTL bounds how long a read at an explicit block is kept. Its value cannot change; this only bounds memory.
const pinnedReadTTL = time.Minute

// NewReadCache is a cache whose reads at "latest" are reused for latestTTL. Zero or less disables reuse but still
// joins identical reads in flight.
func NewReadCache(latestTTL time.Duration) *ReadCache {
	return &ReadCache{latestTTL: latestTTL, now: time.Now, entries: map[string]*readEntry{}}
}

// chainReader is what the client reads the chain with: an ethclient, or one behind a ReadCache.
type chainReader interface {
	contractCaller
	CodeAt(ctx context.Context, account common.Address, block *big.Int) ([]byte, error)
}

// Wrap is `inner` with its contract calls and header reads shared through the cache.
func (r *ReadCache) Wrap(inner chainReader) chainReader {
	return cachedReader{inner: inner, cache: r}
}

type cachedReader struct {
	inner chainReader
	cache *ReadCache
}

func (c cachedReader) CallContract(ctx context.Context, msg ethereum.CallMsg, block *big.Int) ([]byte, error) {
	to := ""
	if msg.To != nil {
		to = msg.To.Hex()
	}
	key := "call|" + msg.From.Hex() + "|" + to + "|" + hex.EncodeToString(msg.Data) + "|" + blockKey(block)
	value, err := c.cache.do(ctx, key, block != nil, func() (any, error) {
		return c.inner.CallContract(ctx, msg, block)
	})
	if err != nil {
		return nil, err
	}
	return value.([]byte), nil
}

func (c cachedReader) HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error) {
	value, err := c.cache.do(ctx, "header|"+blockKey(number), number != nil, func() (any, error) {
		return c.inner.HeaderByNumber(ctx, number)
	})
	if err != nil {
		return nil, err
	}
	return value.(*types.Header), nil
}

// CodeAt is a boot-time check, made once per contract: it is not worth caching.
func (c cachedReader) CodeAt(ctx context.Context, account common.Address, block *big.Int) ([]byte, error) {
	return c.inner.CodeAt(ctx, account, block)
}

func blockKey(block *big.Int) string {
	if block == nil {
		return "latest"
	}
	return block.String()
}

func (r *ReadCache) ttl(pinned bool) time.Duration {
	if pinned {
		return pinnedReadTTL
	}
	return r.latestTTL
}

func (r *ReadCache) do(ctx context.Context, key string, pinned bool, fetch func() (any, error)) (any, error) {
	r.mu.Lock()
	now := r.now()
	if entry, ok := r.entries[key]; ok {
		select {
		case <-entry.done:
			if entry.err == nil && now.Sub(entry.sent) < r.ttl(entry.pinned) {
				r.mu.Unlock()
				return entry.value, nil
			}
		default:
			r.mu.Unlock()
			select {
			case <-entry.done:
				return entry.value, entry.err
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	r.evictLocked(now)
	entry := &readEntry{done: make(chan struct{}), sent: now, pinned: pinned}
	r.entries[key] = entry
	r.mu.Unlock()

	value, err := fetch()

	r.mu.Lock()
	entry.value, entry.err = value, err
	if err != nil && r.entries[key] == entry {
		delete(r.entries, key)
	}
	r.mu.Unlock()
	close(entry.done)
	return value, err
}

// evictLocked drops finished reads past their TTL. The map holds a few dozen keys, so a sweep per new read is cheap.
func (r *ReadCache) evictLocked(now time.Time) {
	for key, entry := range r.entries {
		select {
		case <-entry.done:
			if now.Sub(entry.sent) >= r.ttl(entry.pinned) {
				delete(r.entries, key)
			}
		default:
		}
	}
}
