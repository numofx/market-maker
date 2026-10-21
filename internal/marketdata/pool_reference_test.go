package marketdata

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	gethmath "github.com/ethereum/go-ethereum/common/math"
)

// The selectors the pool reference calls, as `cast sig` derives them. The fake chain below dispatches
// on these literals, so an ABI that packed anything else gets "unknown selector" back.
const (
	selectorObserve = "883bdbfd"
	selectorToken0  = "0dfe1681"
	selectorToken1  = "d21220a7"
)

const (
	testPool = "0xe846373c1a92b167b4e9cd5d8e4d6b1db9e90ec7"
	testEURC = "0x60a3e35cc302bfa44cb288bc5a4f316fdb1adb42"
	testUSDC = "0x833589fcd6edb6e08f4c7c32d4f71b54bda02913"
)

func TestPoolReferenceSelectors(t *testing.T) {
	for name, want := range map[string]string{"observe": selectorObserve, "token0": selectorToken0, "token1": selectorToken1} {
		if got := hex.EncodeToString(poolABI.Methods[name].ID); got != want {
			t.Fatalf("%s selector = %s, cast sig says %s", name, got, want)
		}
	}
}

// fakePool is a Uniswap-v3-style pool and its two tokens behind one JSON-RPC endpoint, dispatching on
// the call's target and selector.
type fakePool struct {
	mu             sync.Mutex
	token0, token1 string
	decimals       map[string]int64
	// tickCumulatives returned by observe([twap, 0]): then, now.
	then, now int64
	observes  int
}

func newEURCPool(meanTick int64, twapSeconds int64) *fakePool {
	return &fakePool{token0: testEURC, token1: testUSDC, decimals: map[string]int64{testEURC: 6, testUSDC: 6},
		then: 86_520_866_080, now: 86_520_866_080 + meanTick*twapSeconds}
}

func signedWord(v int64) string { return hex.EncodeToString(gethmath.U256Bytes(big.NewInt(v))) }

func addressWord(a string) string { return strings.Repeat("0", 24) + strings.TrimPrefix(a, "0x") }

func (p *fakePool) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     any               `json:"id"`
			Params []json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var call struct {
			To   string `json:"to"`
			Data string `json:"data"`
		}
		_ = json.Unmarshal(req.Params[0], &call)
		to, data := strings.ToLower(call.To), strings.TrimPrefix(call.Data, "0x")
		p.mu.Lock()
		defer p.mu.Unlock()
		var result string
		switch {
		case to == testPool && data == selectorToken0:
			result = addressWord(p.token0)
		case to == testPool && data == selectorToken1:
			result = addressWord(p.token1)
		case to == testPool && strings.HasPrefix(data, selectorObserve):
			p.observes++
			// (int56[] tickCumulatives, uint160[] secondsPerLiquidity): two dynamic arrays of two.
			result = signedWord(0x40) + signedWord(0xa0) + signedWord(2) + signedWord(p.then) + signedWord(p.now) +
				signedWord(2) + signedWord(0) + signedWord(0)
		case data == selectorDecimals && p.decimals[to] != 0:
			result = signedWord(p.decimals[to])
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID,
				"error": map[string]any{"code": -32000, "message": fmt.Sprintf("unknown call %s %s", to, data)}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": "0x" + result})
	}))
	t.Cleanup(srv.Close)
	return srv
}

type stubOracle struct {
	price float64
	err   error
}

func (o stubOracle) Name() string { return "chainlink" }
func (o stubOracle) GetAnchorPrice(context.Context, string) (float64, error) {
	return o.price, o.err
}

func banded(srv *httptest.Server, oracle AnchorSource) *PoolBandedAnchorSource {
	return NewPoolBandedAnchorSource(oracle, srv.URL, testPool, 60*time.Second, 50)
}

// The live EURC/USDC Slipstream pool on 2026-10-21: a 300s TWAP tick of 1159, 1.0001^1159 = 1.12290
// USDC per EURC, against Chainlink's 1h EURC/USD at 1.12243 (4 bps apart).
func TestPoolReferenceQuotesThePoolTWAPInsideTheBand(t *testing.T) {
	srv := newEURCPool(1159, 60).serve(t)
	got, err := banded(srv, stubOracle{price: 1.12243}).GetAnchorPrice(context.Background(), "EURC-USDC")
	if err != nil {
		t.Fatalf("GetAnchorPrice: %v", err)
	}
	if want := math.Pow(1.0001, 1159); math.Abs(got-want) > 1e-12 {
		t.Fatalf("price %.8f, want the pool's %.8f, not the oracle's", got, want)
	}
}

// A pool with USDC as token0 quotes token0 per token1 the other way up: the reference inverts it.
func TestPoolReferenceInvertsAPoolWithUSDCFirst(t *testing.T) {
	pool := newEURCPool(-1159, 60)
	pool.token0, pool.token1 = testUSDC, testEURC
	got, err := banded(pool.serve(t), stubOracle{price: 1.12243}).GetAnchorPrice(context.Background(), "EURC-USDC")
	if err != nil {
		t.Fatalf("GetAnchorPrice: %v", err)
	}
	if want := 1 / math.Pow(1.0001, -1159); math.Abs(got-want) > 1e-9 {
		t.Fatalf("price %.8f, want %.8f", got, want)
	}
}

// A pool pushed 100 bps from the oracle is refused, and the error is the one the bot halts on.
func TestPoolReferenceRefusesAPoolOutsideTheBand(t *testing.T) {
	srv := newEURCPool(1159+100, 60).serve(t) // 1.0001^100: ~100 bps above
	_, err := banded(srv, stubOracle{price: 1.12243}).GetAnchorPrice(context.Background(), "EURC-USDC")
	if !errors.Is(err, ErrAnchorOutOfBand) {
		t.Fatalf("err %v, want ErrAnchorOutOfBand", err)
	}
	// Inside the band by a margin it passes: 40 bps.
	if _, err := banded(newEURCPool(1159+36, 60).serve(t), stubOracle{price: 1.12243}).GetAnchorPrice(context.Background(), "EURC-USDC"); err != nil {
		t.Fatalf("40 bps from the oracle refused: %v", err)
	}
}

// The oracle's own refusals reach the bot unchanged: a stale Chainlink round halts it however good the
// pool looks, so the band never runs without its reference point.
func TestPoolReferencePassesTheOraclesRefusalThrough(t *testing.T) {
	pool := newEURCPool(1159, 60)
	stale := fmt.Errorf("%w: round too old", ErrAnchorStale)
	_, err := banded(pool.serve(t), stubOracle{err: stale}).GetAnchorPrice(context.Background(), "EURC-USDC")
	if !errors.Is(err, ErrAnchorStale) {
		t.Fatalf("err %v, want the oracle's ErrAnchorStale", err)
	}
	if pool.observes != 0 {
		t.Fatalf("the pool was read %d times with no oracle to band it", pool.observes)
	}
}

// A pool without USDC on either side cannot give USDC per base.
func TestPoolReferenceRefusesAPoolWithoutUSDC(t *testing.T) {
	pool := newEURCPool(1159, 60)
	pool.token1 = "0x46c85152bfe9f96829aa94755d9f915f9b10ef5f"
	pool.decimals[pool.token1] = 6
	if _, err := banded(pool.serve(t), stubOracle{price: 1.12243}).GetAnchorPrice(context.Background(), "EURC-USDC"); err == nil ||
		!strings.Contains(err.Error(), "neither is USDC") {
		t.Fatalf("err %v, want a refusal naming the missing USDC side", err)
	}
}

// The mean tick rounds toward negative infinity, as Uniswap's OracleLibrary does.
func TestMeanTickRoundsDown(t *testing.T) {
	for _, tc := range []struct{ then, now, seconds, want int64 }{{0, 7, 2, 3}, {0, -7, 2, -4}, {10, 10, 60, 0}, {0, -120, 60, -2}} {
		if got := meanTick(big.NewInt(tc.then), big.NewInt(tc.now), tc.seconds); got != tc.want {
			t.Fatalf("meanTick(%d, %d, %d) = %d, want %d", tc.then, tc.now, tc.seconds, got, tc.want)
		}
	}
}

// Against Base itself, when MM_LIVE_RPC_URL is set: the EURC/USDC Slipstream pool (tick spacing 50)
// banded by Chainlink's 1h EURC/USD feed. Skipped otherwise; CI has no RPC.
func TestPoolReferenceLive(t *testing.T) {
	rpc := strings.TrimSpace(getenv("MM_LIVE_RPC_URL"))
	if rpc == "" {
		t.Skip("MM_LIVE_RPC_URL not set")
	}
	oracle := NewChainlinkAnchorSource(rpc, "0x9867186e52d2F1C2c565CDA6E747101Fa56501e0", 2*time.Hour)
	source := NewPoolBandedAnchorSource(oracle, rpc, testPool, 60*time.Second, 50)
	price, err := source.GetAnchorPrice(context.Background(), "EURC-USDC")
	if err != nil {
		t.Fatalf("live: %v", err)
	}
	reference, _ := oracle.GetAnchorPrice(context.Background(), "EURC-USDC")
	t.Logf("pool 60s TWAP %.6f USDC per EURC; Chainlink 1h %.6f; %.1f bps apart", price, reference, math.Abs(price-reference)/reference*1e4)
	if price < 0.8 || price > 1.6 {
		t.Fatalf("live price %.6f is not a EURC price in USDC", price)
	}
}

var getenv = os.Getenv
