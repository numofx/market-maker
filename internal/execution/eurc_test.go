package execution

import (
	"context"
	"encoding/hex"
	"encoding/json"
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

// eurcFeed is the EURC/USD AggregatorV3 behind a JSON-RPC endpoint, dispatching on the selectors
// `cast sig` gives: decimals() 0x313ce567, latestRoundData() 0xfeaf968c.
type eurcFeed struct {
	mu        sync.Mutex
	answer    int64
	updatedAt int64
}

func (f *eurcFeed) set(answer int64, updatedAt time.Time) {
	f.mu.Lock()
	f.answer, f.updatedAt = answer, updatedAt.Unix()
	f.mu.Unlock()
}

func (f *eurcFeed) serve(t *testing.T) string {
	t.Helper()
	w32 := func(v int64) string { return hex.EncodeToString(gethmath.U256Bytes(big.NewInt(v))) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     any               `json:"id"`
			Params []json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var call struct {
			Data string `json:"data"`
		}
		_ = json.Unmarshal(req.Params[0], &call)
		f.mu.Lock()
		defer f.mu.Unlock()
		var result string
		switch strings.TrimPrefix(call.Data, "0x") {
		case "313ce567":
			result = "0x" + w32(8)
		case "feaf968c":
			result = "0x" + w32(1) + w32(f.answer) + w32(f.updatedAt) + w32(f.updatedAt) + w32(1)
		default:
			http.Error(w, "unknown selector", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func eurcBot(t *testing.T, feed *eurcFeed) (*Bot, *integrationClient) {
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
		OrderSize: 200, HalfSpreadBPS: 20, MaxLongInventory: 100000, MaxShortInventory: -100000, QuoteLevels: 1, LevelSizeMult: 1,
		CancelStaleOrderThreshold: 20, AdoptSizeTolerance: 0.01, OrderExpirySeconds: 60, ExpiryReplaceMarginSeconds: 15,
		AnchorSourceType: config.AnchorSourceChainlink, AnchorChainlinkFeed: "0xDAe398520e2B67cd3f27aeF9Cf14D93D927f8250",
		AnchorChainlinkRPCURL: feed.serve(t), AnchorChainlinkMaxAge: 90000 * time.Second, MaxAnchorDeviationBPS: 50,
	}
	return NewBot(cfg, client, client.spec, metrics.New(), quietLogger(), state.NewStore(cfg.StateFile)), client
}

// End to end through the bot: a EURC-USDC bot on an empty book quotes around the Chainlink round;
// a round past MM_ANCHOR_CHAINLINK_MAX_AGE_SECONDS halts it and cancels its quotes at once (no
// MM_STALE_ANCHOR_TIMEOUT_SECONDS is set here, so nothing but the stale round can be the cause);
// and a book mid more than MM_MAX_ANCHOR_DEVIATION_BPS from a fresh round halts and cancels too.
func TestEURCUSDCBotQuotesAroundTheAnchorAndHaltsOffIt(t *testing.T) {
	feed := &eurcFeed{}
	feed.set(112465583, time.Now().Add(-time.Hour))
	bot, client := eurcBot(t, feed)
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
	for _, p := range client.placed {
		if p.Size != 177 || (p.Side == exchange.SideBuy) != (p.Price < 1.12465583) {
			t.Fatalf("placed %+v; want 177 EURC either side of 1.12465583", p)
		}
	}

	// A stale round: halt and cancel this cycle.
	client.openOrders = []exchange.Order{{ID: "mm:EURC-USDC:buy:1", Side: exchange.SideBuy, Price: 1.1224, Size: 177, Managed: true}}
	feed.set(112465583, time.Now().Add(-90001*time.Second))
	bot2, client2 := eurcBot(t, feed)
	client2.openOrders = client.openOrders
	_ = bot2.Initialize(ctx) // fails closed: the startup load refuses the stale round
	if err := bot2.RunCycle(ctx); err != nil {
		t.Fatalf("RunCycle on a stale round: %v", err)
	}
	if !bot2.currentHalted || bot2.persisted.LastHaltReason != "anchor data stale" || len(client2.cancelled) == 0 {
		t.Fatalf("stale round: halted %v reason %q cancelled %v", bot2.currentHalted, bot2.persisted.LastHaltReason, client2.cancelled)
	}

	// A fresh round and a book 100 bps above it.
	feed.set(112465583, time.Now())
	bot3, client3 := eurcBot(t, feed)
	client3.book = exchange.Book{Bids: []exchange.BookLevel{{Price: 1.1355}}, Asks: []exchange.BookLevel{{Price: 1.1365}}}
	client3.openOrders = client.openOrders
	_ = bot3.Initialize(ctx)
	if err := bot3.RunCycle(ctx); err != nil {
		t.Fatalf("RunCycle off the anchor: %v", err)
	}
	if !bot3.currentHalted || bot3.persisted.LastHaltReason != "anchor deviation exceeded" || len(client3.cancelled) == 0 {
		t.Fatalf("book 100bps off: halted %v reason %q cancelled %v", bot3.currentHalted, bot3.persisted.LastHaltReason, client3.cancelled)
	}
}

func TestCheckAnchorForMarket(t *testing.T) {
	eurc := exchange.MarketSpec{Symbol: "EURC-USDC", Kind: exchange.MarketKindSpot, BaseAsset: "EURC", QuoteAsset: "USDC"}
	cngn := exchange.MarketSpec{Symbol: "cNGN-USDC", Kind: exchange.MarketKindSpot, BaseAsset: "cNGN", QuoteAsset: "USDC"}
	future := exchange.MarketSpec{Symbol: "USDC-cNGN-31DEC26", Kind: exchange.MarketKindFuture}
	chainlink := config.Config{AnchorSourceType: config.AnchorSourceChainlink, MaxAnchorDeviationBPS: 50}
	for name, tc := range map[string]struct {
		cfg  config.Config
		spec exchange.MarketSpec
		ok   bool
	}{
		"EURC with chainlink":                {chainlink, eurc, true},
		"EURC without an anchor":             {config.Config{AnchorSourceType: "none", MaxAnchorDeviationBPS: 50}, eurc, false},
		"EURC with the deviation guard off":  {config.Config{AnchorSourceType: config.AnchorSourceChainlink}, eurc, false},
		"cNGN spot as it runs today":         {config.Config{AnchorSourceType: "none"}, cngn, true},
		"chainlink on the cNGN spot market":  {chainlink, cngn, false},
		"chainlink on a future":              {chainlink, future, false},
		"the future's own anchor, unchanged": {config.Config{AnchorSourceType: "future_index_carry"}, future, true},
	} {
		if err := checkAnchorForMarket(tc.cfg, tc.spec); (err == nil) != tc.ok {
			t.Fatalf("%s: err %v, want ok=%v", name, err, tc.ok)
		}
	}
}
