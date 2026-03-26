package execution

import (
	"context"
	"errors"
	"io"
	"log/slog"
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
		spec: exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "USDC", QuoteAsset: "cNGN", TickSize: 0.01, SizeStep: 0.1, MinSize: 0.1},
		book: exchange.Book{Bids: []exchange.BookLevel{{Price: 99}}, Asks: []exchange.BookLevel{{Price: 101}}},
		balances: []exchange.Balance{
			{Asset: "USDC", Total: 50, Available: 50},
			{Asset: "cNGN", Total: 10000, Available: 10000},
		},
		mockClient: mockClient{
			openOrders: []exchange.Order{
				{ID: "mm:USDCcNGN-SPOT:buy:10", Side: exchange.SideBuy, Price: 99.9, Size: 10, Managed: true, Nonce: "10"},
				{ID: "mm:USDCcNGN-SPOT:sell:11", Side: exchange.SideSell, Price: 100.1, Size: 10, Managed: true, Nonce: "11"},
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
		spec: exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "USDC", QuoteAsset: "cNGN", TickSize: 0.01, SizeStep: 0.1, MinSize: 0.1},
		book: exchange.Book{Bids: []exchange.BookLevel{{Price: 99}}, Asks: []exchange.BookLevel{{Price: 101}}},
		balances: []exchange.Balance{
			{Asset: "USDC", Total: 100, Available: 100},
			{Asset: "cNGN", Total: 100000, Available: 100000},
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
		book: exchange.Book{Bids: []exchange.BookLevel{{Price: 99}}, Asks: []exchange.BookLevel{{Price: 101}}},
		balances: []exchange.Balance{
			{Asset: "USDC", Total: 0, Available: 0},
			{Asset: "cNGN", Total: 100000, Available: 100000},
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
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bot := NewBot(cfg, client, client.spec, metrics.New(), logger, state.NewStore(cfg.StateFile))
	client.placeErr = nil
	if err := bot.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle() error = %v", err)
	}
	client.openOrders = []exchange.Order{{ID: client.placed[0].OrderID, Side: exchange.SideBuy, Nonce: client.placed[0].Nonce, Price: client.placed[0].Price, Size: client.placed[0].Size}}
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
		spec: exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "USDC", QuoteAsset: "cNGN", TickSize: 0.01, SizeStep: 0.1, MinSize: 0.1},
		book: exchange.Book{Bids: []exchange.BookLevel{{Price: 99}}, Asks: []exchange.BookLevel{{Price: 101}}},
		balances: []exchange.Balance{
			{Asset: "USDC", Total: 1, Available: 1},
			{Asset: "cNGN", Total: 5, Available: 5},
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
		spec: exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "USDC", QuoteAsset: "cNGN", TickSize: 0.01, SizeStep: 0.1, MinSize: 0.1},
		balances: []exchange.Balance{
			{Asset: "USDC", Total: 0, Available: 100},
			{Asset: "cNGN", Total: 100000, Available: 100000},
		},
	}
	anchor := &fakeSpotExternalAnchor{quotes: []marketdata.ExternalAnchorQuote{{
		Price:            1500,
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
	bot.loader = marketdata.NewLoaderWithSpotExternal(client, client.spec, marketdata.NewAnchorSource(cfg), anchor, true)
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
		spec: exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "USDC", QuoteAsset: "cNGN", TickSize: 0.01, SizeStep: 0.1, MinSize: 0.1},
		balances: []exchange.Balance{
			{Asset: "USDC", Total: 100, Available: 100},
			{Asset: "cNGN", Total: 100000, Available: 100000},
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
	bot.loader = marketdata.NewLoaderWithSpotExternal(client, client.spec, marketdata.NewAnchorSource(cfg), anchor, true)
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
		spec: exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "USDC", QuoteAsset: "cNGN", TickSize: 0.01, SizeStep: 0.1, MinSize: 0.1},
		balances: []exchange.Balance{
			{Asset: "USDC", Total: 100, Available: 100},
			{Asset: "cNGN", Total: 100000, Available: 100000},
		},
	}
	anchor := &fakeSpotExternalAnchor{quotes: []marketdata.ExternalAnchorQuote{{
		Price:            1500,
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
	bot.loader = marketdata.NewLoaderWithSpotExternal(client, client.spec, marketdata.NewAnchorSource(cfg), anchor, true)
	if err := bot.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle() error = %v", err)
	}
	if bot.snapshot.ReferenceSource != "external" {
		t.Fatalf("first source = %q want external", bot.snapshot.ReferenceSource)
	}
	client.book = exchange.Book{Bids: []exchange.BookLevel{{Price: 1499}}, Asks: []exchange.BookLevel{{Price: 1501}}}
	client.openOrders = nil
	client.placed = nil
	if err := bot.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle() second error = %v", err)
	}
	if bot.snapshot.ReferenceSource != "book" {
		t.Fatalf("second source = %q want book", bot.snapshot.ReferenceSource)
	}
}

func TestPauseModeCancelsAndDoesNotPlace(t *testing.T) {
	client := &integrationClient{
		spec: exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "USDC", QuoteAsset: "cNGN", TickSize: 0.01, SizeStep: 0.1, MinSize: 0.1},
		book: exchange.Book{Bids: []exchange.BookLevel{{Price: 99}}, Asks: []exchange.BookLevel{{Price: 101}}},
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
			spec: exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "USDC", QuoteAsset: "cNGN", TickSize: 0.01, SizeStep: 0.1, MinSize: 0.1},
			book: exchange.Book{Bids: []exchange.BookLevel{{Price: 99}}, Asks: []exchange.BookLevel{{Price: 101}}},
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
		book: exchange.Book{Bids: []exchange.BookLevel{{Price: 99}}, Asks: []exchange.BookLevel{{Price: 101}}},
		balances: []exchange.Balance{
			{Asset: "USDC", Total: 100, Available: 100},
			{Asset: "cNGN", Total: 100000, Available: 100000},
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
		spec: exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "USDC", QuoteAsset: "cNGN", TickSize: 0.01, SizeStep: 0.1, MinSize: 0.1},
		book: exchange.Book{Bids: []exchange.BookLevel{{Price: 99}}, Asks: []exchange.BookLevel{{Price: 101}}},
		balances: []exchange.Balance{
			{Asset: "USDC", Total: 50, Available: 50},
			{Asset: "cNGN", Total: 100000, Available: 100000},
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
	if persisted.LastInventorySnapshot["USDC"] == 0 && persisted.LastInventorySnapshot["cNGN"] == 0 {
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
		spec: exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "USDC", QuoteAsset: "cNGN", TickSize: 0.01, SizeStep: 0.1, MinSize: 0.1},
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
