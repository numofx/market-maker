package execution

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
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

func TestStartupReconciliationCancelsExistingOrders(t *testing.T) {
	client := &integrationClient{
		spec: exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "USDC", QuoteAsset: "cNGN", TickSize: 0.01, SizeStep: 0.1, MinSize: 0.1},
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
		spec: exchange.MarketSpec{Symbol: "USDCcNGN-SPOT", BaseAsset: "USDC", QuoteAsset: "cNGN", TickSize: 0.01, SizeStep: 0.1, MinSize: 0.1},
		book: exchange.Book{Bids: []exchange.BookLevel{{Price: 99}}, Asks: []exchange.BookLevel{{Price: 101}}},
		balances: []exchange.Balance{
			{Asset: "USDC", Total: 100, Available: 100},
			{Asset: "cNGN", Total: 100000, Available: 100000},
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
			openOrders: []exchange.Order{{ID: "live-order", Side: exchange.SideBuy, Nonce: "10"}},
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
	if len(client.cancelled) != 1 {
		t.Fatalf("expected kill switch cancel, got %d", len(client.cancelled))
	}
}
