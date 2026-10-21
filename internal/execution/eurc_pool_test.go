package execution

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gethmath "github.com/ethereum/go-ethereum/common/math"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/metrics"
	"github.com/numofx/market-maker/internal/state"
)

const (
	eurcPoolAddress = "0xe846373c1a92b167b4e9cd5d8e4d6b1db9e90ec7"
	eurcFeedAddress = "0x9867186e52d2f1c2c565cda6e747101fa56501e0"
	eurcToken       = "0x60a3e35cc302bfa44cb288bc5a4f316fdb1adb42"
	usdcToken       = "0x833589fcd6edb6e08f4c7c32d4f71b54bda02913"
)

// eurcChain is the 1h EURC/USD feed, the EURC/USDC pool and both tokens behind one JSON-RPC endpoint,
// dispatching on target and the selectors `cast sig` gives: decimals() 313ce567, latestRoundData()
// feaf968c, token0() 0dfe1681, token1() d21220a7, observe(uint32[]) 883bdbfd.
type eurcChain struct {
	mu       sync.Mutex
	answer   int64 // 8 decimals
	meanTick int64 // the pool's TWAP tick
}

func (c *eurcChain) set(answer, meanTick int64) {
	c.mu.Lock()
	c.answer, c.meanTick = answer, meanTick
	c.mu.Unlock()
}

func (c *eurcChain) serve(t *testing.T) string {
	t.Helper()
	w := func(v int64) string { return hex.EncodeToString(gethmath.U256Bytes(big.NewInt(v))) }
	addr := func(a string) string { return strings.Repeat("0", 24) + strings.TrimPrefix(a, "0x") }
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
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
		c.mu.Lock()
		defer c.mu.Unlock()
		var result string
		switch {
		case to == eurcFeedAddress && data == "313ce567":
			result = w(8)
		case to == eurcFeedAddress && data == "feaf968c":
			now := time.Now().Unix()
			result = w(1) + w(c.answer) + w(now) + w(now) + w(1)
		case (to == eurcToken || to == usdcToken) && data == "313ce567":
			result = w(6)
		case to == eurcPoolAddress && data == "0dfe1681":
			result = addr(eurcToken)
		case to == eurcPoolAddress && data == "d21220a7":
			result = addr(usdcToken)
		case to == eurcPoolAddress && strings.HasPrefix(data, "883bdbfd"):
			result = w(0x40) + w(0xa0) + w(2) + w(0) + w(c.meanTick*60) + w(2) + w(0) + w(0)
		default:
			http.Error(rw, "unknown call", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(rw).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": "0x" + result})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func eurcPoolBot(t *testing.T, rpc string) (*Bot, *integrationClient) {
	t.Helper()
	client := &integrationClient{
		spec: exchange.MarketSpec{Symbol: "EURC-USDC", Kind: exchange.MarketKindSpot, BaseAsset: "EURC", QuoteAsset: "USDC", TickSize: 1e-18, SizeStep: 1, MinSize: 1},
		balances: []exchange.Balance{
			{Asset: "EURC", Total: 1000, Available: 1000},
			{Asset: "USDC", Total: 1000, Available: 1000},
		},
	}
	cfg := config.Config{
		MarketSymbol: "EURC-USDC", StateFile: filepath.Join(t.TempDir(), "state.json"),
		OrderSize: 200, HalfSpreadBPS: 5, MaxLongInventory: 100000, MaxShortInventory: -100000, QuoteLevels: 1, LevelSizeMult: 1,
		CancelStaleOrderThreshold: 20, AdoptSizeTolerance: 0.01, OrderExpirySeconds: 60, ExpiryReplaceMarginSeconds: 15,
		AnchorSourceType: config.AnchorSourceChainlink, AnchorChainlinkFeed: eurcFeedAddress,
		AnchorChainlinkRPCURL: rpc, AnchorChainlinkMaxAge: 2 * time.Hour, MaxAnchorDeviationBPS: 50,
		AnchorPoolAddress: eurcPoolAddress, AnchorPoolTWAP: 60 * time.Second, AnchorBandBPS: 50,
	}
	return NewBot(cfg, client, client.spec, metrics.New(), quietLogger(), state.NewStore(cfg.StateFile)), client
}

// End to end with the live reference: a EURC-USDC bot quotes around the pool's TWAP, not the Chainlink
// answer (here 15 bps apart, as the 24h feed and the market were on 2026-10-21); and with the pool
// pushed 100 bps off Chainlink it halts with "anchor out of band" and cancels, on its first cycle.
func TestEURCUSDCBotQuotesAroundThePoolInsideTheChainlinkBand(t *testing.T) {
	chain := &eurcChain{}
	chain.set(112465583, 1159) // Chainlink 1.124656, pool 1.0001^1159 = 1.122897
	rpc := chain.serve(t)
	bot, client := eurcPoolBot(t, rpc)
	ctx := context.Background()
	if err := bot.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := bot.RunCycle(ctx); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	if bot.currentHalted || len(client.placed) != 2 {
		t.Fatalf("halted %v (%q), placed %+v; want a bid and an ask", bot.currentHalted, bot.persisted.LastHaltReason, client.placed)
	}
	pool := math.Pow(1.0001, 1159)
	for _, p := range client.placed {
		if (p.Side == exchange.SideBuy) != (p.Price < pool) || math.Abs(p.Price-pool)/pool*1e4 > 6 {
			t.Fatalf("placed %+v; want 5 bps either side of the pool's %.6f, not Chainlink's 1.124656", p, pool)
		}
	}

	chain.set(112465583, 1274) // 1.0001^1274 = 1.135893: 100 bps above Chainlink's 1.124656
	bot2, client2 := eurcPoolBot(t, rpc)
	client2.openOrders = []exchange.Order{{ID: "mm:EURC-USDC:buy:1", Side: exchange.SideBuy, Price: 1.1224, Size: 177, Managed: true}}
	_ = bot2.Initialize(ctx) // the startup load refuses the out-of-band reference too
	if err := bot2.RunCycle(ctx); err != nil {
		t.Fatalf("RunCycle out of band: %v", err)
	}
	if !bot2.currentHalted || bot2.persisted.LastHaltReason != "anchor out of band" || len(client2.cancelled) == 0 {
		t.Fatalf("pool out of band: halted %v reason %q cancelled %v", bot2.currentHalted, bot2.persisted.LastHaltReason, client2.cancelled)
	}
}
