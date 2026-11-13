package exchange

import (
	"context"
	"errors"
	"math/big"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// countingReader counts the reads that reach the RPC. gate, when set, holds every call until it is closed.
type countingReader struct {
	calls   atomic.Int64
	headers atomic.Int64
	gate    chan struct{}
	err     error
}

func (r *countingReader) CallContract(_ context.Context, msg ethereum.CallMsg, _ *big.Int) ([]byte, error) {
	r.calls.Add(1)
	if r.gate != nil {
		<-r.gate
	}
	if r.err != nil {
		return nil, r.err
	}
	return append([]byte{0xAA}, msg.Data...), nil
}

func (r *countingReader) HeaderByNumber(_ context.Context, number *big.Int) (*types.Header, error) {
	r.headers.Add(1)
	if number == nil {
		number = big.NewInt(100)
	}
	return &types.Header{Number: number}, nil
}

func (r *countingReader) CodeAt(context.Context, common.Address, *big.Int) ([]byte, error) {
	return []byte{1}, nil
}

func newTestCache(ttl time.Duration) (*ReadCache, *time.Time) {
	now := time.Unix(1_800_000_000, 0)
	cache := NewReadCache(ttl)
	cache.now = func() time.Time { return now }
	return cache, &now
}

func callMsg(data byte) ethereum.CallMsg {
	to := common.HexToAddress("0x7019244E25FA416e6Ca2ed2F3cA25277aef72843")
	return ethereum.CallMsg{To: &to, Data: []byte{data}}
}

// Ten strikes reading the same thing in one cycle make one RPC call, and each still gets the answer.
func TestReadCacheJoinsConcurrentIdenticalReads(t *testing.T) {
	inner := &countingReader{gate: make(chan struct{})}
	cache, _ := newTestCache(time.Second)
	var wg sync.WaitGroup
	results := make([][]byte, 10)
	for i := range results {
		reader := cache.Wrap(inner)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, err := reader.CallContract(context.Background(), callMsg(7), nil)
			if err != nil {
				t.Errorf("strike %d: %v", i, err)
			}
			results[i] = out
		}(i)
	}
	for inner.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(inner.gate)
	wg.Wait()
	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("RPC calls = %d, want 1", got)
	}
	for i, out := range results {
		if len(out) != 2 || out[1] != 7 {
			t.Fatalf("strike %d got %x", i, out)
		}
	}
}

// A read at latest is reused only while fresh, so the next cycle sees new state.
func TestReadCacheLatestExpires(t *testing.T) {
	inner := &countingReader{}
	cache, now := newTestCache(time.Second)
	reader := cache.Wrap(inner)
	ctx := context.Background()
	for range 3 {
		if _, err := reader.CallContract(ctx, callMsg(1), nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("within the TTL: RPC calls = %d, want 1", got)
	}
	*now = now.Add(time.Second)
	if _, err := reader.CallContract(ctx, callMsg(1), nil); err != nil {
		t.Fatal(err)
	}
	if got := inner.calls.Load(); got != 2 {
		t.Fatalf("after the TTL: RPC calls = %d, want 2", got)
	}
	// Different calldata is a different read.
	if _, err := reader.CallContract(ctx, callMsg(2), nil); err != nil {
		t.Fatal(err)
	}
	if got := inner.calls.Load(); got != 3 {
		t.Fatalf("other calldata: RPC calls = %d, want 3", got)
	}
}

// A read at an explicit block outlives the latest TTL: its value cannot change.
func TestReadCachePinnedOutlivesLatest(t *testing.T) {
	inner := &countingReader{}
	cache, now := newTestCache(time.Second)
	reader := cache.Wrap(inner)
	ctx := context.Background()
	block := big.NewInt(42)
	if _, err := reader.CallContract(ctx, callMsg(1), block); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(10 * time.Second)
	if _, err := reader.CallContract(ctx, callMsg(1), big.NewInt(42)); err != nil {
		t.Fatal(err)
	}
	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("RPC calls = %d, want 1", got)
	}
	if _, err := reader.CallContract(ctx, callMsg(1), big.NewInt(43)); err != nil {
		t.Fatal(err)
	}
	if got := inner.calls.Load(); got != 2 {
		t.Fatalf("another block: RPC calls = %d, want 2", got)
	}
}

// A failed read is not reused: the next caller asks again.
func TestReadCacheDoesNotKeepErrors(t *testing.T) {
	inner := &countingReader{err: errors.New("429 Too Many Requests")}
	cache, _ := newTestCache(time.Second)
	reader := cache.Wrap(inner)
	for range 2 {
		if _, err := reader.CallContract(context.Background(), callMsg(1), nil); err == nil {
			t.Fatal("want the RPC error")
		}
	}
	if got := inner.calls.Load(); got != 2 {
		t.Fatalf("RPC calls = %d, want 2", got)
	}
}

func TestReadCacheSharesLatestHeader(t *testing.T) {
	inner := &countingReader{}
	cache, now := newTestCache(time.Second)
	reader := cache.Wrap(inner)
	for range 3 {
		header, err := reader.HeaderByNumber(context.Background(), nil)
		if err != nil || header.Number.Int64() != 100 {
			t.Fatalf("header %v, %v", header, err)
		}
	}
	if got := inner.headers.Load(); got != 1 {
		t.Fatalf("header reads = %d, want 1", got)
	}
	*now = now.Add(time.Second)
	if _, err := reader.HeaderByNumber(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if got := inner.headers.Load(); got != 2 {
		t.Fatalf("header reads = %d, want 2", got)
	}
}
