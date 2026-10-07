package execution

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/marketdata"
	"github.com/numofx/market-maker/internal/metrics"
	"github.com/numofx/market-maker/internal/state"
)

type integrationClient struct {
	mockClient
	book     exchange.Book
	trades   []exchange.Trade
	balances []exchange.Balance
	spec     exchange.MarketSpec
}

func (c *integrationClient) GetBook(context.Context, string) (exchange.Book, error) {
	return c.book, nil
}
func (c *integrationClient) GetTrades(context.Context, string) ([]exchange.Trade, error) {
	return c.trades, nil
}
func (c *integrationClient) GetBalances(context.Context) ([]exchange.Balance, error) {
	return c.balances, nil
}
func (c *integrationClient) GetMarket(context.Context, string) (exchange.MarketSpec, error) {
	return c.spec, nil
}

type fakeSpotExternalAnchor struct {
	quotes []marketdata.ExternalAnchorQuote
	idx    int
}

func (f *fakeSpotExternalAnchor) Fetch(context.Context) marketdata.ExternalAnchorQuote {
	if len(f.quotes) == 0 {
		return marketdata.ExternalAnchorQuote{}
	}
	if f.idx >= len(f.quotes) {
		return f.quotes[len(f.quotes)-1]
	}
	quote := f.quotes[f.idx]
	f.idx++
	return quote
}

func TestStartupReconciliationCancelsExistingOrders(t *testing.T) {
	client := &integrationClient{
		spec: exchange.MarketSpec{Symbol: "USDCcNGN-APR30-2026", BaseAsset: "USDC", QuoteAsset: "cNGN", TickSize: 0.01, SizeStep: 0.1, MinSize: 0.1},
		mockClient: mockClient{
			openOrders: []exchange.Order{
				{ID: "mm:USDCcNGN-SPOT:buy:1", Side: exchange.SideBuy, Nonce: "1", Managed: true},
				{ID: "manual-order", Side: exchange.SideSell, Nonce: "2", Managed: false},
			},
		},
	}
	cfg := config.Config{MarketSymbol: "USDCcNGN-SPOT", StateFile: filepath.Join(t.TempDir(), "state.json")}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bot := NewBot(cfg, client, client.spec, metrics.New(), logger, state.NewStore(cfg.StateFile))
	if err := bot.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	if len(client.cancelled) != 2 {
		t.Fatalf("startup cancels = %d want 2", len(client.cancelled))
	}
}

func TestInitializeAdoptsExistingManagedQuotesAndNextCycleDoesNotDuplicate(t *testing.T) {
	client := &integrationClient{
		spec: exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "cNGN", QuoteAsset: "USDC", TickSize: 0.0001, SizeStep: 1, MinSize: 1},
		book: exchange.Book{Bids: []exchange.BookLevel{{Price: 0.99}}, Asks: []exchange.BookLevel{{Price: 1.01}}},
		balances: []exchange.Balance{
			{Asset: "cNGN", Total: 50, Available: 50},
			{Asset: "USDC", Total: 10000, Available: 10000},
		},
		mockClient: mockClient{
			openOrders: []exchange.Order{
				{ID: "mm:USDCcNGN-SPOT:buy:10", Side: exchange.SideBuy, Price: 0.999, Size: 10, Managed: true, Nonce: "10"},
				{ID: "mm:USDCcNGN-SPOT:sell:11", Side: exchange.SideSell, Price: 1.001, Size: 10, Managed: true, Nonce: "11"},
			},
		},
	}
	cfg := config.Config{
		MarketSymbol:              "USDCcNGN-SPOT",
		StateFile:                 filepath.Join(t.TempDir(), "state.json"),
		OrderSize:                 10,
		HalfSpreadBPS:             10,
		MaxLongInventory:          100,
		MaxShortInventory:         -100,
		QuoteRefreshInterval:      0,
		CancelStaleOrderThreshold: 20,
		AdoptSizeTolerance:        0.01,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bot := NewBot(cfg, client, client.spec, metrics.New(), logger, state.NewStore(cfg.StateFile))
	if err := bot.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	if len(client.cancelled) != 0 {
		t.Fatalf("unexpected cancels during adoption: %d", len(client.cancelled))
	}
	if err := bot.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle() error = %v", err)
	}
	if len(client.placed) != 0 {
		t.Fatalf("expected no duplicate placements after adoption, got %d", len(client.placed))
	}
}

func TestCancelReplaceAfterRestartAllocatesNewNonces(t *testing.T) {
	client := &integrationClient{
		spec: exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "cNGN", QuoteAsset: "USDC", TickSize: 0.0001, SizeStep: 1, MinSize: 1},
		book: exchange.Book{Bids: []exchange.BookLevel{{Price: 0.99}}, Asks: []exchange.BookLevel{{Price: 1.01}}},
		balances: []exchange.Balance{
			{Asset: "cNGN", Total: 50, Available: 50},
			{Asset: "USDC", Total: 100000, Available: 100000},
		},
		mockClient: mockClient{
			openOrders: []exchange.Order{{ID: "old-bid", Side: exchange.SideBuy, Nonce: "5"}},
		},
	}
	cfg := config.Config{
		MarketSymbol:         "USDCcNGN-SPOT",
		StateFile:            filepath.Join(t.TempDir(), "state.json"),
		OrderSize:            10,
		HalfSpreadBPS:        10,
		MaxLongInventory:     200,
		MaxShortInventory:    -100,
		QuoteRefreshInterval: 0,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bot := NewBot(cfg, client, client.spec, metrics.New(), logger, state.NewStore(cfg.StateFile))
	if err := bot.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	client.openOrders = nil
	if err := bot.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle() error = %v", err)
	}
	if len(client.placed) != 2 {
		t.Fatalf("placements = %d want 2", len(client.placed))
	}
	if client.placed[0].Nonce == "5" || client.placed[1].Nonce == "5" {
		t.Fatal("expected new nonces after restart reconciliation")
	}
}

func TestNoDuplicateQuotesOnPartialFailure(t *testing.T) {
	client := &integrationClient{
		spec: exchange.MarketSpec{Symbol: "USDCcNGN-APR30-2026", BaseAsset: "USDC", QuoteAsset: "cNGN", TickSize: 0.01, SizeStep: 0.1, MinSize: 0.1},
		book: exchange.Book{Bids: []exchange.BookLevel{{Price: 0.99}}, Asks: []exchange.BookLevel{{Price: 1.01}}},
		balances: []exchange.Balance{
			{Asset: "USDC", Total: 0, Available: 0},
			{Asset: "cNGN", Total: 100000, Available: 100000},
		},
	}
	cfg := config.Config{
		MarketSymbol:              "USDCcNGN-APR30-2026",
		StateFile:                 filepath.Join(t.TempDir(), "state.json"),
		OrderSize:                 10,
		HalfSpreadBPS:             10,
		MaxLongInventory:          100,
		MaxShortInventory:         -100,
		QuoteRefreshInterval:      0,
		CancelStaleOrderThreshold: 20,
		AdoptSizeTolerance:        0.01,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bot := NewBot(cfg, client, client.spec, metrics.New(), logger, state.NewStore(cfg.StateFile))
	client.placeErr = nil
	if err := bot.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle() error = %v", err)
	}
	// The future is cash-margined, so cycle 1 quotes BOTH sides even with zero base
	// inventory. Simulate a partial failure where only the bid actually rested; the
	// live bid must be adopted (not duplicated) while the missing ask is placed.
	client.openOrders = []exchange.Order{{ID: client.placed[0].OrderID, Side: exchange.SideBuy, Nonce: client.placed[0].Nonce, Price: client.placed[0].Price, Size: client.placed[0].Size, Managed: true}}
	before := len(client.placed)
	if err := bot.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle() second error = %v", err)
	}
	if len(client.placed) > before+1 {
		t.Fatalf("unexpected duplicate placements: before=%d after=%d", before, len(client.placed))
	}
}

func TestHaltedWhenBalancesInsufficient(t *testing.T) {
	client := &integrationClient{
		spec: exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "cNGN", QuoteAsset: "USDC", TickSize: 0.0001, SizeStep: 1, MinSize: 1},
		book: exchange.Book{Bids: []exchange.BookLevel{{Price: 0.99}}, Asks: []exchange.BookLevel{{Price: 1.01}}},
		balances: []exchange.Balance{
			{Asset: "cNGN", Total: 5, Available: 5},
			{Asset: "USDC", Total: 1, Available: 1},
		},
		mockClient: mockClient{
			openOrders: []exchange.Order{
				{ID: "live-bid", Side: exchange.SideBuy, Nonce: "10"},
				{ID: "live-ask", Side: exchange.SideSell, Nonce: "11"},
			},
		},
	}
	cfg := config.Config{
		MarketSymbol:         "USDCcNGN-SPOT",
		StateFile:            filepath.Join(t.TempDir(), "state.json"),
		OrderSize:            10,
		HalfSpreadBPS:        10,
		MaxLongInventory:     100,
		MaxShortInventory:    -100,
		MinBaseBalance:       10,
		MinQuoteBalance:      100,
		QuoteRefreshInterval: 0,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bot := NewBot(cfg, client, client.spec, metrics.New(), logger, state.NewStore(cfg.StateFile))
	if err := bot.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle() error = %v", err)
	}
	if len(client.cancelled) != 2 {
		t.Fatalf("expected kill switch cancel-all, got %d", len(client.cancelled))
	}
}

func TestEmptySpotMarketUsesFreshExternalAnchor(t *testing.T) {
	client := &integrationClient{
		spec: exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "cNGN", QuoteAsset: "USDC", TickSize: 0.0001, SizeStep: 1, MinSize: 1},
		balances: []exchange.Balance{
			{Asset: "cNGN", Total: 20000, Available: 20000},
			{Asset: "USDC", Total: 100000, Available: 100000},
		},
	}
	anchor := &fakeSpotExternalAnchor{quotes: []marketdata.ExternalAnchorQuote{{
		Price:            1 / 1500.0,
		Present:          true,
		FetchedAt:        time.Now().UTC(),
		RefreshAttempted: true,
	}}}
	cfg := config.Config{
		MarketSymbol:         "USDCcNGN-SPOT",
		StateFile:            filepath.Join(t.TempDir(), "state.json"),
		OrderSize:            10,
		HalfSpreadBPS:        10,
		MaxLongInventory:     100,
		MaxShortInventory:    -100,
		QuoteRefreshInterval: 0,
		USDCCNGNSpotExternalAnchor: config.USDCCNGNSpotExternalAnchorConfig{
			Enabled:          true,
			Provider:         "0x",
			BaseURL:          "https://example.invalid/price",
			ChainID:          8453,
			SellToken:        "0xsell",
			BuyToken:         "0xbuy",
			Amount:           "1000000",
			Timeout:          time.Second,
			MaxAge:           time.Minute,
			MaxDeviationBPS:  500,
			BootstrapOnly:    true,
			SpreadMultiplier: 2,
			SizeMultiplier:   0.5,
		},
	}
	reg := metrics.New()
	bot := NewBot(cfg, client, client.spec, reg, slog.New(slog.NewTextHandler(io.Discard, nil)), state.NewStore(cfg.StateFile))
	bot.loader = marketdata.NewLoaderWithSpotExternal(client, client.spec, marketdata.NewAnchorSource(cfg, client.spec), anchor, true)
	if err := bot.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle() error = %v", err)
	}
	if bot.snapshot.ReferenceSource != "external" {
		t.Fatalf("reference source = %q want external", bot.snapshot.ReferenceSource)
	}
	if len(client.placed) != 2 {
		t.Fatalf("placements = %d want 2", len(client.placed))
	}
	rr := httptest.NewRecorder()
	reg.ReadyHandler().ServeHTTP(rr, httptest.NewRequest("GET", "/readyz", nil))
	if rr.Code != 200 {
		t.Fatalf("readyz = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestEmptySpotMarketInvalidExternalAnchorHalts(t *testing.T) {
	client := &integrationClient{
		spec: exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "cNGN", QuoteAsset: "USDC", TickSize: 0.0001, SizeStep: 1, MinSize: 1},
		balances: []exchange.Balance{
			{Asset: "cNGN", Total: 20000, Available: 20000},
			{Asset: "USDC", Total: 100000, Available: 100000},
		},
	}
	anchor := &fakeSpotExternalAnchor{quotes: []marketdata.ExternalAnchorQuote{{
		RefreshAttempted: true,
		RefreshFailed:    true,
	}}}
	cfg := config.Config{
		MarketSymbol:         "USDCcNGN-SPOT",
		StateFile:            filepath.Join(t.TempDir(), "state.json"),
		OrderSize:            10,
		HalfSpreadBPS:        10,
		MaxLongInventory:     100,
		MaxShortInventory:    -100,
		QuoteRefreshInterval: 0,
		USDCCNGNSpotExternalAnchor: config.USDCCNGNSpotExternalAnchorConfig{
			Enabled:          true,
			Provider:         "0x",
			BaseURL:          "https://example.invalid/price",
			ChainID:          8453,
			SellToken:        "0xsell",
			BuyToken:         "0xbuy",
			Amount:           "1000000",
			Timeout:          time.Second,
			MaxAge:           time.Minute,
			MaxDeviationBPS:  500,
			BootstrapOnly:    true,
			SpreadMultiplier: 2,
			SizeMultiplier:   0.5,
		},
	}
	reg := metrics.New()
	bot := NewBot(cfg, client, client.spec, reg, slog.New(slog.NewTextHandler(io.Discard, nil)), state.NewStore(cfg.StateFile))
	bot.loader = marketdata.NewLoaderWithSpotExternal(client, client.spec, marketdata.NewAnchorSource(cfg, client.spec), anchor, true)
	if err := bot.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle() error = %v", err)
	}
	if !bot.currentHalted {
		t.Fatal("expected halted bot")
	}
	if bot.persisted.LastHaltReason != "reference price unavailable" {
		t.Fatalf("halt reason = %q", bot.persisted.LastHaltReason)
	}
}

func TestBootstrapOnlySwitchesFromExternalToLocal(t *testing.T) {
	client := &integrationClient{
		spec: exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "cNGN", QuoteAsset: "USDC", TickSize: 0.0001, SizeStep: 1, MinSize: 1},
		balances: []exchange.Balance{
			{Asset: "cNGN", Total: 20000, Available: 20000},
			{Asset: "USDC", Total: 100000, Available: 100000},
		},
	}
	anchor := &fakeSpotExternalAnchor{quotes: []marketdata.ExternalAnchorQuote{{
		Price:            1 / 1500.0,
		Present:          true,
		FetchedAt:        time.Now().UTC(),
		RefreshAttempted: true,
	}}}
	cfg := config.Config{
		MarketSymbol:         "USDCcNGN-SPOT",
		StateFile:            filepath.Join(t.TempDir(), "state.json"),
		OrderSize:            10,
		HalfSpreadBPS:        10,
		MaxLongInventory:     100,
		MaxShortInventory:    -100,
		QuoteRefreshInterval: 0,
		USDCCNGNSpotExternalAnchor: config.USDCCNGNSpotExternalAnchorConfig{
			Enabled:          true,
			Provider:         "0x",
			BaseURL:          "https://example.invalid/price",
			ChainID:          8453,
			SellToken:        "0xsell",
			BuyToken:         "0xbuy",
			Amount:           "1000000",
			Timeout:          time.Second,
			MaxAge:           time.Minute,
			MaxDeviationBPS:  500,
			BootstrapOnly:    true,
			SpreadMultiplier: 2,
			SizeMultiplier:   0.5,
		},
	}
	bot := NewBot(cfg, client, client.spec, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)), state.NewStore(cfg.StateFile))
	bot.loader = marketdata.NewLoaderWithSpotExternal(client, client.spec, marketdata.NewAnchorSource(cfg, client.spec), anchor, true)
	if err := bot.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle() error = %v", err)
	}
	if bot.snapshot.ReferenceSource != "external" {
		t.Fatalf("first source = %q want external", bot.snapshot.ReferenceSource)
	}
	client.book = exchange.Book{Bids: []exchange.BookLevel{{Price: 1 / 1501.0}}, Asks: []exchange.BookLevel{{Price: 1 / 1499.0}}}
	client.openOrders = nil
	client.placed = nil
	if err := bot.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle() second error = %v", err)
	}
	if bot.snapshot.ReferenceSource != "book" {
		t.Fatalf("second source = %q want book", bot.snapshot.ReferenceSource)
	}
}

// Live on 2026-09-14: a trader's two orders were the whole book, a mid 196 bps from the cNGN
// oracle. With the oracle as the spot anchor that tripped the 150 bps deviation guard, cancelled
// everything and stayed halted. The book is spot's source of truth: the bot prices off that mid,
// and quotes the one side it can fund -- here the ask, since it holds cNGN and dust USDC.
func TestSpotQuotesOffTheBookWhenTheOracleDisagrees(t *testing.T) {
	const bookBid, bookAsk = 0.000730, 0.000750
	client := &integrationClient{
		spec:   exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "cNGN", QuoteAsset: "USDC", TickSize: 0.000000001, SizeStep: 1, MinSize: 1},
		book:   exchange.Book{Bids: []exchange.BookLevel{{Price: bookBid}}, Asks: []exchange.BookLevel{{Price: bookAsk}}},
		trades: []exchange.Trade{{Price: 0.000753, CreatedAt: time.Now().UTC().Add(-10 * time.Minute)}},
		balances: []exchange.Balance{
			{Asset: "cNGN", Total: 8980, Available: 8980},
			{Asset: "USDC", Total: 0.000682, Available: 0.000682},
		},
	}
	anchor := &fakeSpotExternalAnchor{quotes: []marketdata.ExternalAnchorQuote{{
		Price:            0.000755,
		Present:          true,
		FetchedAt:        time.Now().UTC().Add(-20 * time.Minute),
		RefreshAttempted: true,
	}}}
	cfg := config.Config{
		MarketSymbol:          "USDCcNGN-SPOT",
		StateFile:             filepath.Join(t.TempDir(), "state.json"),
		OrderSize:             1.2,
		HalfSpreadBPS:         10,
		QuoteLevels:           5,
		LevelSpreadStepBPS:    15,
		LevelSizeMult:         1.2,
		MaxNetInventory:       60,
		MaxNotionalPerSide:    15000,
		MaxAnchorDeviationBPS: 150,
		StaleAnchorTimeout:    time.Minute,
		QuoteRefreshInterval:  0,
		USDCCNGNSpotExternalAnchor: config.USDCCNGNSpotExternalAnchorConfig{
			Enabled:          true,
			Provider:         "cngn-price-oracle",
			ChainID:          8453,
			Timeout:          time.Second,
			MaxAge:           time.Hour,
			MaxDeviationBPS:  100,
			BootstrapOnly:    true,
			SpreadMultiplier: 2,
			SizeMultiplier:   0.5,
		},
	}
	bot := NewBot(cfg, client, client.spec, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)), state.NewStore(cfg.StateFile))
	bot.loader = marketdata.NewLoaderWithSpotExternal(client, client.spec, marketdata.NewAnchorSource(cfg, client.spec), anchor, true)
	if err := bot.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle() error = %v", err)
	}
	if bot.currentHalted {
		t.Fatalf("halted: %q; an oracle that disagrees with the book must not stop spot quoting", bot.persisted.LastHaltReason)
	}
	if bot.snapshot.ReferenceSource != "book" || math.Abs(bot.snapshot.ReferencePrice-(bookBid+bookAsk)/2) > 1e-15 {
		t.Fatalf("reference = %v (%s), want the book mid %v", bot.snapshot.ReferencePrice, bot.snapshot.ReferenceSource, (bookBid+bookAsk)/2)
	}
	if len(client.placed) == 0 {
		t.Fatal("placed nothing; the cNGN-funded ask side should quote")
	}
	for _, order := range client.placed {
		if order.Side != exchange.SideSell || order.Size < 1 || order.Size != math.Floor(order.Size) {
			t.Fatalf("placed %+v; 0.000682 USDC cannot fund a bid, and sizes are whole cNGN", order)
		}
	}
}

// Live after #21 deployed: the bot's own order was the best on its side opposite a trader's, so
// each re-quote moved the mid it priced from and walked toward the trader. The reference is now
// the trader's orders alone. In engine terms the bot's order is an ask (a sell of cNGN at
// 0.000732) against the trader's bid at 0.000730 and ask at 0.000750.
func TestSpotReferenceIgnoresTheBotsOwnQuotes(t *testing.T) {
	ownAsk := exchange.Order{ID: "mm:USDCcNGN-SPOT:sell:1", Market: "USDCcNGN-SPOT", Side: exchange.SideSell, Price: 0.000732, Size: 1600, Managed: true}
	for _, tt := range []struct {
		name       string
		book       exchange.Book
		wantRef    float64
		wantSource string
	}{
		{
			name: "trader on both sides",
			book: exchange.Book{
				Bids: []exchange.BookLevel{{Price: 0.000730, OrderID: "spot-trader-bid"}},
				Asks: []exchange.BookLevel{{Price: 0.000732, OrderID: ownAsk.ID}, {Price: 0.000750, OrderID: "spot-trader-ask"}},
			},
			wantRef:    0.000740,
			wantSource: "book",
		},
		{
			// Others quote one side only, so there is no two-sided book to take a mid from.
			name: "trader bid only falls back to the last trade",
			book: exchange.Book{
				Bids: []exchange.BookLevel{{Price: 0.000730, OrderID: "spot-trader-bid"}},
				Asks: []exchange.BookLevel{{Price: 0.000732, OrderID: ownAsk.ID}},
			},
			wantRef:    0.000753,
			wantSource: "trade",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := &integrationClient{
				mockClient: mockClient{openOrders: []exchange.Order{ownAsk}},
				spec:       exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "cNGN", QuoteAsset: "USDC", TickSize: 0.000000001, SizeStep: 1, MinSize: 1},
				book:       tt.book,
				trades:     []exchange.Trade{{Price: 0.000753, CreatedAt: time.Now().UTC().Add(-2 * time.Hour)}},
				balances: []exchange.Balance{
					{Asset: "cNGN", Total: 8980, Available: 8980},
					{Asset: "USDC", Total: 0.000682, Available: 0.000682},
				},
			}
			cfg := config.Config{
				MarketSymbol:         "USDCcNGN-SPOT",
				StateFile:            filepath.Join(t.TempDir(), "state.json"),
				OrderSize:            1.2,
				HalfSpreadBPS:        10,
				QuoteLevels:          5,
				LevelSpreadStepBPS:   15,
				LevelSizeMult:        1.2,
				MaxNetInventory:      60,
				MaxNotionalPerSide:   15000,
				QuoteRefreshInterval: 0,
			}
			bot := NewBot(cfg, client, client.spec, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)), state.NewStore(cfg.StateFile))
			if err := bot.RunCycle(context.Background()); err != nil {
				t.Fatalf("RunCycle() error = %v", err)
			}
			if bot.snapshot.ReferenceSource != tt.wantSource || math.Abs(bot.snapshot.ReferencePrice-tt.wantRef) > 1e-15 {
				t.Fatalf("reference = %v (%s), want %v (%s)", bot.snapshot.ReferencePrice, bot.snapshot.ReferenceSource, tt.wantRef, tt.wantSource)
			}
		})
	}
}

// After #22 deployed, with only a trader's order opposite the bot, the reference fell back to the
// venue's last trade (hours old) while the market had moved. The rate picker's price now comes
// first. In engine terms: the bot's own ask at 0.000740, a trader's bid at 0.000730, an old trade
// at 0.000753, and the rate picker at 0.000729.
func TestSpotFallsBackToTheRatePickerBeforeTheLastTrade(t *testing.T) {
	ownAsk := exchange.Order{ID: "mm:USDCcNGN-SPOT:sell:1", Market: "USDCcNGN-SPOT", Side: exchange.SideSell, Price: 0.000740, Size: 1600, Managed: true}
	client := &integrationClient{
		mockClient: mockClient{openOrders: []exchange.Order{ownAsk}},
		spec:       exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "cNGN", QuoteAsset: "USDC", TickSize: 0.000000001, SizeStep: 1, MinSize: 1},
		book: exchange.Book{
			Bids: []exchange.BookLevel{{Price: 0.000730, OrderID: "spot-trader-bid"}},
			Asks: []exchange.BookLevel{{Price: 0.000740, OrderID: ownAsk.ID}},
		},
		trades: []exchange.Trade{{Price: 0.000753, CreatedAt: time.Now().UTC().Add(-3 * time.Hour)}},
		balances: []exchange.Balance{
			{Asset: "cNGN", Total: 8980, Available: 8980},
			{Asset: "USDC", Total: 0.000682, Available: 0.000682},
		},
	}
	anchor := &fakeSpotExternalAnchor{quotes: []marketdata.ExternalAnchorQuote{{
		Price:            0.000729,
		Present:          true,
		FetchedAt:        time.Now().UTC(),
		RefreshAttempted: true,
	}}}
	cfg := config.Config{
		MarketSymbol:         "USDCcNGN-SPOT",
		StateFile:            filepath.Join(t.TempDir(), "state.json"),
		OrderSize:            1.2,
		HalfSpreadBPS:        10,
		QuoteLevels:          5,
		LevelSpreadStepBPS:   15,
		LevelSizeMult:        1.2,
		MaxNetInventory:      60,
		MaxNotionalPerSide:   15000,
		QuoteRefreshInterval: 0,
		USDCCNGNSpotExternalAnchor: config.USDCCNGNSpotExternalAnchorConfig{
			Enabled:          true,
			Provider:         marketdata.RatePickerProvider,
			Timeout:          3 * time.Second,
			MaxAge:           15 * time.Minute,
			MaxDeviationBPS:  100,
			BootstrapOnly:    true,
			SpreadMultiplier: 2,
			SizeMultiplier:   0.5,
		},
	}
	bot := NewBot(cfg, client, client.spec, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)), state.NewStore(cfg.StateFile))
	bot.loader = marketdata.NewLoaderWithSpotExternal(client, client.spec, marketdata.NewAnchorSource(cfg, client.spec), anchor, true)
	if err := bot.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle() error = %v", err)
	}
	if bot.snapshot.ReferenceSource != "external" || bot.snapshot.ReferencePrice != 0.000729 {
		t.Fatalf("reference = %v (%s), want the rate picker's 0.000729", bot.snapshot.ReferencePrice, bot.snapshot.ReferenceSource)
	}
	if len(client.placed) == 0 {
		t.Fatal("placed nothing; the cNGN-funded ask side should quote above the fallback price")
	}
	for _, order := range client.placed {
		if order.Side != exchange.SideSell || order.Price <= 0.000729 {
			t.Fatalf("placed %+v; want only asks above the fallback price", order)
		}
	}
}

func TestPauseModeCancelsAndDoesNotPlace(t *testing.T) {
	client := &integrationClient{
		spec: exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "cNGN", QuoteAsset: "USDC", TickSize: 0.0001, SizeStep: 1, MinSize: 1},
		book: exchange.Book{Bids: []exchange.BookLevel{{Price: 0.99}}, Asks: []exchange.BookLevel{{Price: 1.01}}},
		balances: []exchange.Balance{
			{Asset: "cNGN", Total: 50, Available: 50},
			{Asset: "USDC", Total: 100000, Available: 100000},
		},
		mockClient: mockClient{
			openOrders: []exchange.Order{
				{ID: "live-bid", Side: exchange.SideBuy, Nonce: "10"},
				{ID: "live-ask", Side: exchange.SideSell, Nonce: "11"},
			},
		},
	}
	cfg := config.Config{
		MarketSymbol:         "USDCcNGN-SPOT",
		StateFile:            filepath.Join(t.TempDir(), "state.json"),
		OrderSize:            10,
		HalfSpreadBPS:        10,
		MaxLongInventory:     100,
		MaxShortInventory:    -100,
		QuoteRefreshInterval: 0,
		OperatorMode:         config.ModePause,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bot := NewBot(cfg, client, client.spec, metrics.New(), logger, state.NewStore(cfg.StateFile))
	if err := bot.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle() error = %v", err)
	}
	if len(client.cancelled) != 2 {
		t.Fatalf("pause mode cancels = %d want 2", len(client.cancelled))
	}
	if len(client.placed) != 0 {
		t.Fatalf("pause mode placements = %d want 0", len(client.placed))
	}
}

// Pause must also be honored on startup (Initialize), not only in RunCycle — otherwise a restarting
// paused bot places a fresh ladder from nothing on every boot before the first cycle, so pause
// silently fails as a kill. With no pre-existing orders, Initialize WOULD reconcile-and-place; under
// pause it must place nothing. (This is the real incident: a crash-looping paused futures bot
// re-quoted on each restart.)
func TestPauseModeInitializeDoesNotPlaceFromEmpty(t *testing.T) {
	client := &integrationClient{
		spec: exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "cNGN", QuoteAsset: "USDC", TickSize: 0.0001, SizeStep: 1, MinSize: 1},
		book: exchange.Book{Bids: []exchange.BookLevel{{Price: 0.99}}, Asks: []exchange.BookLevel{{Price: 1.01}}},
		balances: []exchange.Balance{
			{Asset: "USDC", Total: 100, Available: 100},
			{Asset: "cNGN", Total: 100000, Available: 100000},
		},
		// No pre-existing orders: without the pause fix, ReconcileStartup places a fresh ladder.
	}
	cfg := config.Config{
		MarketSymbol:      "USDCcNGN-SPOT",
		StateFile:         filepath.Join(t.TempDir(), "state.json"),
		OrderSize:         10,
		HalfSpreadBPS:     10,
		MaxLongInventory:  100,
		MaxShortInventory: -100,
		OperatorMode:      config.ModePause,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bot := NewBot(cfg, client, client.spec, metrics.New(), logger, state.NewStore(cfg.StateFile))
	if err := bot.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	// Under pause, Initialize must take the halt path (cancel + idle), not the reconcile path that
	// places a fresh ladder. currentHalted==true iff it halted; a reconciling startup sets it false.
	if !bot.Summary().Halted {
		t.Fatal("paused startup did not halt — pause not honored on startup, would place a ladder")
	}
	if len(client.placed) != 0 {
		t.Fatalf("paused startup placed %d orders, want 0", len(client.placed))
	}
}

type failingLoadClient struct {
	integrationClient
	bookErr    error
	balanceErr error
}

func (c *failingLoadClient) GetBook(context.Context, string) (exchange.Book, error) {
	if c.bookErr != nil {
		return exchange.Book{}, c.bookErr
	}
	return c.integrationClient.GetBook(context.Background(), "")
}

func (c *failingLoadClient) GetBalances(context.Context) ([]exchange.Balance, error) {
	if c.balanceErr != nil {
		return nil, c.balanceErr
	}
	return c.integrationClient.GetBalances(context.Background())
}

func TestStaleDependencyLoadErrorCancelsManagedOrders(t *testing.T) {
	client := &failingLoadClient{
		integrationClient: integrationClient{
			spec: exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "cNGN", QuoteAsset: "USDC", TickSize: 0.0001, SizeStep: 1, MinSize: 1},
			book: exchange.Book{Bids: []exchange.BookLevel{{Price: 0.99}}, Asks: []exchange.BookLevel{{Price: 1.01}}},
			balances: []exchange.Balance{
				{Asset: "USDC", Total: 100, Available: 100},
				{Asset: "cNGN", Total: 100000, Available: 100000},
			},
			mockClient: mockClient{
				openOrders: []exchange.Order{
					{ID: "live-bid", Side: exchange.SideBuy, Nonce: "10"},
					{ID: "live-ask", Side: exchange.SideSell, Nonce: "11"},
				},
			},
		},
	}
	cfg := config.Config{
		MarketSymbol:           "USDCcNGN-SPOT",
		StateFile:              filepath.Join(t.TempDir(), "state.json"),
		OrderSize:              10,
		HalfSpreadBPS:          10,
		MaxLongInventory:       100,
		MaxShortInventory:      -100,
		QuoteRefreshInterval:   0,
		StaleMarketDataTimeout: time.Second,
		StaleBalanceTimeout:    time.Second,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bot := NewBot(cfg, client, client.spec, metrics.New(), logger, state.NewStore(cfg.StateFile))
	bot.snapshot = state.Snapshot{
		LastMarketDataRefresh: time.Now().UTC().Add(-2 * time.Second),
		LastBalanceRefresh:    time.Now().UTC().Add(-2 * time.Second),
	}
	client.bookErr = errors.New("book unavailable")
	if err := bot.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle() error = %v", err)
	}
	if len(client.cancelled) != 2 {
		t.Fatalf("stale dependency cancels = %d want 2", len(client.cancelled))
	}
}

func TestStaleAnchorIsolatedFromExchangeMarketData(t *testing.T) {
	client := &integrationClient{
		spec: exchange.MarketSpec{Symbol: "USDCcNGN-APR30-2026", BaseAsset: "USDC", QuoteAsset: "cNGN", TickSize: 0.01, SizeStep: 0.1, MinSize: 0.1},
		book: exchange.Book{Bids: []exchange.BookLevel{{Price: 0.99}}, Asks: []exchange.BookLevel{{Price: 1.01}}},
		balances: []exchange.Balance{
			{Asset: "cNGN", Total: 50, Available: 50},
			{Asset: "USDC", Total: 100000, Available: 100000},
		},
		mockClient: mockClient{
			openOrders: []exchange.Order{{ID: "live-bid", Side: exchange.SideBuy, Nonce: "10"}},
		},
	}
	cfg := config.Config{
		MarketSymbol:         "USDCcNGN-APR30-2026",
		StateFile:            filepath.Join(t.TempDir(), "state.json"),
		OrderSize:            10,
		HalfSpreadBPS:        10,
		MaxLongInventory:     100,
		MaxShortInventory:    -100,
		QuoteRefreshInterval: 0,
		AnchorSourceType:     "http",
		AnchorURL:            "http://127.0.0.1:1",
		StaleAnchorTimeout:   time.Second,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := metrics.New()
	bot := NewBot(cfg, client, client.spec, reg, logger, state.NewStore(cfg.StateFile))
	bot.snapshot = state.Snapshot{
		LastMarketDataRefresh: time.Now().UTC(),
		LastBalanceRefresh:    time.Now().UTC(),
		LastAnchorRefresh:     time.Now().UTC().Add(-2 * time.Second),
		AnchorSource:          "http",
		AnchorPrice:           100,
		InventoryByAsset:      map[string]float64{"USDC": 0},
	}

	if err := bot.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle() error = %v", err)
	}
	if len(client.cancelled) != 1 {
		t.Fatalf("cancelled = %d want 1", len(client.cancelled))
	}
	rr := httptest.NewRecorder()
	reg.ReadyHandler().ServeHTTP(rr, httptest.NewRequest("GET", "/readyz", nil))
	if rr.Code != 503 || !strings.Contains(rr.Body.String(), "anchor data stale") {
		t.Fatalf("ready = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestPersistedStateSurvivesRestart(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	client := &integrationClient{
		spec: exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "cNGN", QuoteAsset: "USDC", TickSize: 0.0001, SizeStep: 1, MinSize: 1},
		book: exchange.Book{Bids: []exchange.BookLevel{{Price: 0.99}}, Asks: []exchange.BookLevel{{Price: 1.01}}},
		balances: []exchange.Balance{
			{Asset: "cNGN", Total: 50, Available: 50},
			{Asset: "USDC", Total: 100000, Available: 100000},
		},
	}
	cfg := config.Config{
		MarketSymbol:         "USDCcNGN-SPOT",
		StateFile:            stateFile,
		OrderSize:            10,
		HalfSpreadBPS:        10,
		MaxLongInventory:     100,
		MaxShortInventory:    -100,
		QuoteRefreshInterval: 0,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bot := NewBot(cfg, client, client.spec, metrics.New(), logger, state.NewStore(cfg.StateFile))
	if err := bot.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle() error = %v", err)
	}

	persisted, err := state.NewStore(stateFile).Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if persisted.LastSubmittedBidOrder == "" || persisted.LastSubmittedAskOrder == "" {
		t.Fatalf("submitted ids missing: %#v", persisted)
	}
	if persisted.LastInventorySnapshot["cNGN"] == 0 && persisted.LastInventorySnapshot["USDC"] == 0 {
		t.Fatalf("inventory snapshot missing: %#v", persisted.LastInventorySnapshot)
	}
}

func TestKillSwitchCancelsAllAndHaltsQuoting(t *testing.T) {
	dir := t.TempDir()
	killFile := filepath.Join(dir, "kill.switch")
	if err := os.WriteFile(killFile, []byte("1"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	stateFile := filepath.Join(dir, "state.json")
	client := &integrationClient{
		spec: exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "cNGN", QuoteAsset: "USDC", TickSize: 0.0001, SizeStep: 1, MinSize: 1},
		mockClient: mockClient{
			openOrders: []exchange.Order{
				{ID: "live-bid", Side: exchange.SideBuy, Nonce: "10"},
				{ID: "live-ask", Side: exchange.SideSell, Nonce: "11"},
			},
		},
	}
	cfg := config.Config{
		MarketSymbol:   "USDCcNGN-SPOT",
		StateFile:      stateFile,
		KillSwitchFile: killFile,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bot := NewBot(cfg, client, client.spec, metrics.New(), logger, state.NewStore(cfg.StateFile))
	if err := bot.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle() error = %v", err)
	}
	if len(client.cancelled) != 2 {
		t.Fatalf("cancelled = %d want 2", len(client.cancelled))
	}
	persisted, err := state.NewStore(stateFile).Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if persisted.LastHaltReason != "kill switch active" {
		t.Fatalf("LastHaltReason = %q", persisted.LastHaltReason)
	}
}
