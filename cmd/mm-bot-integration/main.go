package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
	"github.com/numofx/market-maker/internal/execution"
	"github.com/numofx/market-maker/internal/metrics"
	"github.com/numofx/market-maker/internal/state"
)

type integrationConfig struct {
	TakerOwnerPrivateKey  string
	TakerSignerPrivateKey string
	TakerOwnerAddress     string
	TakerSignerAddress    string
	TakerSubaccountID     string
	TakerRecipientID      string
	Timeout               time.Duration
	PollInterval          time.Duration
	Scenario              string
}

type summary struct {
	Scenario              string         `json:"scenario"`
	OpenBidCount          int            `json:"open_bid_count"`
	OpenAskCount          int            `json:"open_ask_count"`
	LastTradePresent      bool           `json:"last_trade_present"`
	InventoryBefore       float64        `json:"inventory_before"`
	InventoryAfter        float64        `json:"inventory_after"`
	AdoptedOrderIDs       []string       `json:"adopted_order_ids_after_restart"`
	NoDuplicateSideOrders bool           `json:"no_duplicate_side_orders"`
	Extra                 map[string]any `json:"extra,omitempty"`
}

type environment struct {
	cfg         config.Config
	intCfg      integrationConfig
	logger      *slog.Logger
	botClient   *exchange.HTTPClient
	takerClient *exchange.HTTPClient
	spec        exchange.MarketSpec
	store       *state.Store
}

type runResult struct {
	summary summary
}

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load bot config", "error", err)
		os.Exit(1)
	}
	intCfg, err := loadIntegrationConfig()
	if err != nil {
		slog.Error("load integration config", "error", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)

	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(rootCtx, intCfg.Timeout)
	defer cancel()

	env, err := newEnvironment(ctx, cfg, intCfg, logger)
	if err != nil {
		logger.Error("init environment", "error", err)
		os.Exit(1)
	}
	defer env.botClient.Close()
	defer env.takerClient.Close()

	var result runResult
	switch intCfg.Scenario {
	case "happy":
		result, err = runHappyPath(ctx, env)
	case "partial_fill":
		result, err = runPartialFill(ctx, env)
	case "restart_under_open_order":
		result, err = runRestartUnderOpenOrder(ctx, env)
	case "stale_startup":
		result, err = runStaleStartup(ctx, env)
	case "duplicate_startup":
		result, err = runDuplicateStartup(ctx, env)
	default:
		err = fmt.Errorf("unsupported MM_INT_SCENARIO %q", intCfg.Scenario)
	}
	if err != nil {
		logger.Error("integration scenario failed", "scenario", intCfg.Scenario, "error", err)
		os.Exit(1)
	}

	encoded, err := json.MarshalIndent(result.summary, "", "  ")
	if err != nil {
		logger.Error("marshal summary failed", "error", err)
		os.Exit(1)
	}
	fmt.Println(string(encoded))
}

func newEnvironment(ctx context.Context, cfg config.Config, intCfg integrationConfig, logger *slog.Logger) (*environment, error) {
	botClient, err := exchange.NewHTTPClient(ctx, exchange.ClientConfig{
		APIBaseURL:         cfg.APIBaseURL,
		RPCURL:             cfg.RPCURL,
		DatabaseURL:        cfg.DatabaseURL,
		MarketSymbol:       cfg.MarketSymbol,
		ChainID:            cfg.ChainID,
		MatchingRepoPath:   cfg.MatchingRepoPath,
		RiskCoreRepoPath:   cfg.RiskCoreRepoPath,
		MatchingAddress:    cfg.MatchingAddress,
		TradeModuleAddress: cfg.TradeModuleAddress,
		SubAccountsAddress: cfg.SubAccountsAddress,
		OwnerAddress:       cfg.OwnerAddress,
		SignerAddress:      cfg.SignerAddress,
		OwnerPrivateKey:    cfg.OwnerPrivateKey,
		SignerPrivateKey:   cfg.SignerPrivateKey,
		SubaccountID:       cfg.SubaccountID,
		RecipientID:        cfg.RecipientID,
		WorstFee:           cfg.WorstFee,
		OrderExpirySeconds: cfg.OrderExpirySeconds,
	})
	if err != nil {
		return nil, fmt.Errorf("init bot client: %w", err)
	}
	takerClient, err := exchange.NewHTTPClient(ctx, exchange.ClientConfig{
		APIBaseURL:         cfg.APIBaseURL,
		RPCURL:             cfg.RPCURL,
		DatabaseURL:        cfg.DatabaseURL,
		MarketSymbol:       cfg.MarketSymbol,
		ChainID:            cfg.ChainID,
		MatchingRepoPath:   cfg.MatchingRepoPath,
		RiskCoreRepoPath:   cfg.RiskCoreRepoPath,
		MatchingAddress:    cfg.MatchingAddress,
		TradeModuleAddress: cfg.TradeModuleAddress,
		SubAccountsAddress: cfg.SubAccountsAddress,
		OwnerAddress:       intCfg.TakerOwnerAddress,
		SignerAddress:      intCfg.TakerSignerAddress,
		OwnerPrivateKey:    intCfg.TakerOwnerPrivateKey,
		SignerPrivateKey:   intCfg.TakerSignerPrivateKey,
		SubaccountID:       intCfg.TakerSubaccountID,
		RecipientID:        intCfg.TakerRecipientID,
		WorstFee:           cfg.WorstFee,
		OrderExpirySeconds: cfg.OrderExpirySeconds,
	})
	if err != nil {
		botClient.Close()
		return nil, fmt.Errorf("init taker client: %w", err)
	}
	spec, err := botClient.GetMarket(ctx, cfg.MarketSymbol)
	if err != nil {
		botClient.Close()
		takerClient.Close()
		return nil, fmt.Errorf("resolve market: %w", err)
	}
	return &environment{
		cfg:         cfg,
		intCfg:      intCfg,
		logger:      logger,
		botClient:   botClient,
		takerClient: takerClient,
		spec:        spec,
		store:       state.NewStore(cfg.StateFile),
	}, nil
}

func runHappyPath(ctx context.Context, env *environment) (runResult, error) {
	if err := ensureCleanMarket(ctx, env, true); err != nil {
		return runResult{}, err
	}
	_, stopLoop, errCh, err := startBotLoop(ctx, env)
	if err != nil {
		return runResult{}, err
	}
	defer stopBotLoop(stopLoop, errCh)

	initialOrders, err := waitForSingleQuotes(ctx, env, "wait_initial_quotes")
	if err != nil {
		return runResult{}, err
	}
	inventoryBefore, err := readInventory(ctx, env.botClient, env.spec.BaseAsset)
	if err != nil {
		return runResult{}, err
	}
	_, ask := splitSides(initialOrders)
	if err := submitTakerCross(ctx, env, ask.Price, ask.Size, "cross_full_ask"); err != nil {
		return runResult{}, err
	}
	if err := waitForTrade(ctx, env, "wait_fill"); err != nil {
		return runResult{}, err
	}
	inventoryAfter, err := waitForInventoryChange(ctx, env, env.spec.BaseAsset, inventoryBefore, "wait_inventory_change")
	if err != nil {
		return runResult{}, err
	}
	if _, err := waitForSingleQuotesWithNewAsk(ctx, env, ask.ID, "wait_requote"); err != nil {
		return runResult{}, err
	}
	stopBotLoop(stopLoop, errCh)
	restartResult, postRestartOrders, err := restartAndVerifyAdoption(ctx, env, "restart_happy")
	if err != nil {
		return runResult{}, err
	}
	trades, _ := env.botClient.GetTrades(ctx, env.spec.Symbol)
	return runResult{summary: summary{
		Scenario:              "happy",
		OpenBidCount:          countSide(postRestartOrders, exchange.SideBuy),
		OpenAskCount:          countSide(postRestartOrders, exchange.SideSell),
		LastTradePresent:      len(trades) > 0,
		InventoryBefore:       inventoryBefore,
		InventoryAfter:        inventoryAfter,
		AdoptedOrderIDs:       nonEmpty([]string{restartResult.AdoptedBidOrderID, restartResult.AdoptedAskOrderID}),
		NoDuplicateSideOrders: noDuplicateSideOrders(postRestartOrders),
	}}, nil
}

func runPartialFill(ctx context.Context, env *environment) (runResult, error) {
	if err := ensureCleanMarket(ctx, env, true); err != nil {
		return runResult{}, err
	}
	_, stopLoop, errCh, err := startBotLoop(ctx, env)
	if err != nil {
		return runResult{}, err
	}
	defer stopBotLoop(stopLoop, errCh)

	initialOrders, err := waitForSingleQuotes(ctx, env, "wait_initial_quotes")
	if err != nil {
		return runResult{}, err
	}
	_, ask := splitSides(initialOrders)
	inventoryBefore, err := readInventory(ctx, env.botClient, env.spec.BaseAsset)
	if err != nil {
		return runResult{}, err
	}
	partialSize := ask.Size / 2
	if partialSize <= 0 {
		return runResult{}, fmt.Errorf("calculated partial size <= 0")
	}
	if err := submitTakerCross(ctx, env, ask.Price, partialSize, "cross_partial_ask"); err != nil {
		return runResult{}, err
	}
	if err := waitForTrade(ctx, env, "wait_partial_fill_trade"); err != nil {
		return runResult{}, err
	}
	inventoryAfter, err := waitForInventoryChange(ctx, env, env.spec.BaseAsset, inventoryBefore, "wait_partial_inventory_change")
	if err != nil {
		return runResult{}, err
	}
	ordersAfter, err := waitForQuotesState(ctx, env, "wait_partial_order_state", func(orders []exchange.Order) bool {
		return countSide(orders, exchange.SideBuy) == 1 && countSide(orders, exchange.SideSell) == 1
	})
	if err != nil {
		return runResult{}, err
	}
	_, askAfter := splitSides(ordersAfter)
	if askAfter.Size <= 0 || askAfter.Size > ask.Size {
		return runResult{}, fmt.Errorf("unexpected ask remaining size after partial fill: before=%f after=%f", ask.Size, askAfter.Size)
	}
	if askAfter.ID != ask.ID && askAfter.Size >= ask.Size {
		return runResult{}, fmt.Errorf("partial fill replacement produced invalid ask state: before=%s after=%s", ask.ID, askAfter.ID)
	}
	trades, _ := env.botClient.GetTrades(ctx, env.spec.Symbol)
	return runResult{summary: summary{
		Scenario:              "partial_fill",
		OpenBidCount:          countSide(ordersAfter, exchange.SideBuy),
		OpenAskCount:          countSide(ordersAfter, exchange.SideSell),
		LastTradePresent:      len(trades) > 0,
		InventoryBefore:       inventoryBefore,
		InventoryAfter:        inventoryAfter,
		NoDuplicateSideOrders: noDuplicateSideOrders(ordersAfter),
		Extra: map[string]any{
			"partial_fill_size": partialSize,
			"ask_before_id":     ask.ID,
			"ask_after_id":      askAfter.ID,
			"ask_before_size":   ask.Size,
			"ask_after_size":    askAfter.Size,
		},
	}}, nil
}

func runRestartUnderOpenOrder(ctx context.Context, env *environment) (runResult, error) {
	if err := ensureCleanMarket(ctx, env, true); err != nil {
		return runResult{}, err
	}
	_, stopLoop, errCh, err := startBotLoop(ctx, env)
	if err != nil {
		return runResult{}, err
	}
	initialOrders, err := waitForSingleQuotes(ctx, env, "wait_initial_quotes")
	if err != nil {
		stopBotLoop(stopLoop, errCh)
		return runResult{}, err
	}
	stopBotLoop(stopLoop, errCh)

	recon, postRestartOrders, err := restartAndVerifyAdoption(ctx, env, "restart_under_open_order")
	if err != nil {
		return runResult{}, err
	}
	_, stopLoop2, errCh2, err := startBotLoop(ctx, env)
	if err != nil {
		return runResult{}, err
	}
	defer stopBotLoop(stopLoop2, errCh2)
	stableOrders, err := waitForQuotesState(ctx, env, "wait_stable_post_restart", func(orders []exchange.Order) bool {
		return noDuplicateSideOrders(orders) && sameOrderSet(orders, postRestartOrders)
	})
	if err != nil {
		return runResult{}, err
	}
	return runResult{summary: summary{
		Scenario:              "restart_under_open_order",
		OpenBidCount:          countSide(stableOrders, exchange.SideBuy),
		OpenAskCount:          countSide(stableOrders, exchange.SideSell),
		AdoptedOrderIDs:       nonEmpty([]string{recon.AdoptedBidOrderID, recon.AdoptedAskOrderID}),
		NoDuplicateSideOrders: noDuplicateSideOrders(stableOrders),
		Extra: map[string]any{
			"orders_before_restart": orderIDs(initialOrders),
			"orders_after_restart":  orderIDs(stableOrders),
		},
	}}, nil
}

func runStaleStartup(ctx context.Context, env *environment) (runResult, error) {
	if err := ensureCleanMarket(ctx, env, true); err != nil {
		return runResult{}, err
	}
	staleOrders, err := seedManagedOrders(ctx, env, []seedOrder{
		{Side: exchange.SideBuy, Price: 1, Size: env.cfg.OrderSize, Nonce: "900001"},
		{Side: exchange.SideSell, Price: 999999, Size: env.cfg.OrderSize, Nonce: "900002"},
	})
	if err != nil {
		return runResult{}, err
	}
	restartBot := execution.NewBot(env.cfg, env.botClient, env.spec, metrics.New(), env.logger, env.store)
	if err := restartBot.Initialize(ctx); err != nil {
		return runResult{}, err
	}
	recon := restartBot.LastReconciliationResult()
	if len(recon.CanceledOrderIDs) < 2 {
		return runResult{}, fmt.Errorf("expected stale startup orders to be canceled, got %#v", recon)
	}
	_, stopLoop, errCh, err := startSpecificBotLoop(ctx, env, restartBot)
	if err != nil {
		return runResult{}, err
	}
	defer stopBotLoop(stopLoop, errCh)
	freshOrders, err := waitForSingleQuotes(ctx, env, "wait_fresh_quotes_after_stale_startup")
	if err != nil {
		return runResult{}, err
	}
	return runResult{summary: summary{
		Scenario:              "stale_startup",
		OpenBidCount:          countSide(freshOrders, exchange.SideBuy),
		OpenAskCount:          countSide(freshOrders, exchange.SideSell),
		AdoptedOrderIDs:       nonEmpty([]string{recon.AdoptedBidOrderID, recon.AdoptedAskOrderID}),
		NoDuplicateSideOrders: noDuplicateSideOrders(freshOrders),
		Extra: map[string]any{
			"seeded_order_ids":   staleOrders,
			"canceled_order_ids": recon.CanceledOrderIDs,
		},
	}}, nil
}

func runDuplicateStartup(ctx context.Context, env *environment) (runResult, error) {
	if err := ensureCleanMarket(ctx, env, true); err != nil {
		return runResult{}, err
	}
	duplicateOrders, err := seedManagedOrders(ctx, env, []seedOrder{
		{Side: exchange.SideBuy, Price: 99, Size: env.cfg.OrderSize, Nonce: "910001"},
		{Side: exchange.SideBuy, Price: 98.9, Size: env.cfg.OrderSize, Nonce: "910002"},
		{Side: exchange.SideSell, Price: 101, Size: env.cfg.OrderSize, Nonce: "910003"},
	})
	if err != nil {
		return runResult{}, err
	}
	restartBot := execution.NewBot(env.cfg, env.botClient, env.spec, metrics.New(), env.logger, env.store)
	if err := restartBot.Initialize(ctx); err != nil {
		return runResult{}, err
	}
	recon := restartBot.LastReconciliationResult()
	if len(recon.CanceledOrderIDs) < 2 {
		return runResult{}, fmt.Errorf("expected duplicate startup orders to be canceled, got %#v", recon)
	}
	_, stopLoop, errCh, err := startSpecificBotLoop(ctx, env, restartBot)
	if err != nil {
		return runResult{}, err
	}
	defer stopBotLoop(stopLoop, errCh)
	orders, err := waitForSingleQuotes(ctx, env, "wait_quotes_after_duplicate_cleanup")
	if err != nil {
		return runResult{}, err
	}
	if !noDuplicateSideOrders(orders) {
		return runResult{}, fmt.Errorf("duplicate side orders still present after startup reconciliation")
	}
	return runResult{summary: summary{
		Scenario:              "duplicate_startup",
		OpenBidCount:          countSide(orders, exchange.SideBuy),
		OpenAskCount:          countSide(orders, exchange.SideSell),
		NoDuplicateSideOrders: noDuplicateSideOrders(orders),
		Extra: map[string]any{
			"seeded_order_ids":   duplicateOrders,
			"canceled_order_ids": recon.CanceledOrderIDs,
		},
	}}, nil
}

type seedOrder struct {
	Side  exchange.Side
	Price float64
	Size  float64
	Nonce string
}

func seedManagedOrders(ctx context.Context, env *environment, items []seedOrder) ([]string, error) {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		orderID := fmt.Sprintf("mm:%s:%s:%s", env.spec.Symbol, item.Side, item.Nonce)
		_, err := env.botClient.PlaceLimitOrder(ctx, exchange.PlaceOrderRequest{
			Market:  env.spec.Symbol,
			Side:    item.Side,
			Price:   item.Price,
			Size:    item.Size,
			OrderID: orderID,
			Nonce:   item.Nonce,
		})
		if err != nil {
			return nil, fmt.Errorf("seed order %s: %w", orderID, err)
		}
		ids = append(ids, orderID)
	}
	return ids, nil
}

func restartAndVerifyAdoption(ctx context.Context, env *environment, phase string) (execution.ReconciliationResult, []exchange.Order, error) {
	env.logger.Info("phase", "name", phase)
	restartBot := execution.NewBot(env.cfg, env.botClient, env.spec, metrics.New(), env.logger, env.store)
	if err := restartBot.Initialize(ctx); err != nil {
		return execution.ReconciliationResult{}, nil, fmt.Errorf("%s initialize: %w", phase, err)
	}
	recon := restartBot.LastReconciliationResult()
	orders, err := waitForSingleQuotes(ctx, env, phase+"_quotes")
	if err != nil {
		return execution.ReconciliationResult{}, nil, err
	}
	if !noDuplicateSideOrders(orders) {
		return execution.ReconciliationResult{}, nil, fmt.Errorf("%s duplicate side orders detected", phase)
	}
	return recon, orders, nil
}

func startBotLoop(ctx context.Context, env *environment) (*execution.Bot, context.CancelFunc, chan error, error) {
	bot := execution.NewBot(env.cfg, env.botClient, env.spec, metrics.New(), env.logger, env.store)
	if err := bot.Initialize(ctx); err != nil {
		return nil, nil, nil, fmt.Errorf("initialize bot: %w", err)
	}
	return startSpecificBotLoop(ctx, env, bot)
}

func startSpecificBotLoop(ctx context.Context, env *environment, bot *execution.Bot) (*execution.Bot, context.CancelFunc, chan error, error) {
	botLoopCtx, stopLoop := context.WithCancel(ctx)
	errCh := make(chan error, 1)
	go runBotLoop(botLoopCtx, bot, env.cfg.PollInterval, errCh)
	return bot, stopLoop, errCh, nil
}

func stopBotLoop(stopLoop context.CancelFunc, errCh chan error) {
	if stopLoop == nil {
		return
	}
	stopLoop()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("bot loop exited with error", "error", err)
		}
	case <-time.After(2 * time.Second):
	}
}

func loadIntegrationConfig() (integrationConfig, error) {
	timeoutSeconds := envInt("MM_INT_TIMEOUT_SECONDS", 120)
	pollMS := envInt("MM_INT_POLL_INTERVAL_MS", 500)
	cfg := integrationConfig{
		TakerOwnerPrivateKey:  stringsTrim("MM_INT_TAKER_OWNER_PRIVATE_KEY"),
		TakerSignerPrivateKey: defaultEnv("MM_INT_TAKER_SIGNER_PRIVATE_KEY", os.Getenv("MM_INT_TAKER_OWNER_PRIVATE_KEY")),
		TakerOwnerAddress:     stringsTrim("MM_INT_TAKER_OWNER_ADDRESS"),
		TakerSignerAddress:    stringsTrim("MM_INT_TAKER_SIGNER_ADDRESS"),
		TakerSubaccountID:     stringsTrim("MM_INT_TAKER_SUBACCOUNT_ID"),
		TakerRecipientID:      defaultEnv("MM_INT_TAKER_RECIPIENT_ID", os.Getenv("MM_INT_TAKER_SUBACCOUNT_ID")),
		Timeout:               time.Duration(timeoutSeconds) * time.Second,
		PollInterval:          time.Duration(pollMS) * time.Millisecond,
		Scenario:              defaultEnv("MM_INT_SCENARIO", "happy"),
	}
	if cfg.TakerOwnerPrivateKey == "" {
		return integrationConfig{}, fmt.Errorf("MM_INT_TAKER_OWNER_PRIVATE_KEY is required")
	}
	if cfg.TakerSubaccountID == "" {
		return integrationConfig{}, fmt.Errorf("MM_INT_TAKER_SUBACCOUNT_ID is required")
	}
	return cfg, nil
}

func ensureCleanMarket(ctx context.Context, env *environment, requireEmptyBook bool) error {
	env.logger.Info("phase", "name", "clean_market", "scenario", env.intCfg.Scenario)
	if err := env.botClient.CancelAllOrders(ctx, env.spec.Symbol); err != nil {
		return fmt.Errorf("cancel bot orders: %w", err)
	}
	if err := env.takerClient.CancelAllOrders(ctx, env.spec.Symbol); err != nil {
		return fmt.Errorf("cancel taker orders: %w", err)
	}
	if err := waitForStage(ctx, env, "wait_own_orders_cleared", func() (bool, map[string]any, error) {
		botOrders, err := env.botClient.ListOpenOrders(ctx, env.spec.Symbol)
		if err != nil {
			return false, nil, err
		}
		takerOrders, err := env.takerClient.ListOpenOrders(ctx, env.spec.Symbol)
		if err != nil {
			return false, nil, err
		}
		detail := map[string]any{"bot_orders": len(botOrders), "taker_orders": len(takerOrders)}
		return len(botOrders) == 0 && len(takerOrders) == 0, detail, nil
	}); err != nil {
		return err
	}
	if !requireEmptyBook {
		return nil
	}
	book, err := env.botClient.GetBook(ctx, env.spec.Symbol)
	if err != nil {
		return fmt.Errorf("load book after cleanup: %w", err)
	}
	if len(book.Bids) > 0 || len(book.Asks) > 0 {
		return fmt.Errorf("market book is not empty after cleanup; bids=%d asks=%d; use an isolated environment or empty market first", len(book.Bids), len(book.Asks))
	}
	return nil
}

func runBotLoop(ctx context.Context, bot *execution.Bot, interval time.Duration, errCh chan<- error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			errCh <- ctx.Err()
			return
		case <-ticker.C:
			if err := bot.RunCycle(ctx); err != nil {
				errCh <- err
				return
			}
		}
	}
}

func waitForSingleQuotes(ctx context.Context, env *environment, stage string) ([]exchange.Order, error) {
	return waitForQuotesState(ctx, env, stage, func(orders []exchange.Order) bool {
		return countSide(orders, exchange.SideBuy) == 1 && countSide(orders, exchange.SideSell) == 1
	})
}

func waitForSingleQuotesWithNewAsk(ctx context.Context, env *environment, previousAskID string, stage string) ([]exchange.Order, error) {
	return waitForQuotesState(ctx, env, stage, func(orders []exchange.Order) bool {
		if countSide(orders, exchange.SideBuy) != 1 || countSide(orders, exchange.SideSell) != 1 {
			return false
		}
		_, ask := splitSides(orders)
		return ask.ID != "" && ask.ID != previousAskID
	})
}

func waitForQuotesState(ctx context.Context, env *environment, stage string, predicate func([]exchange.Order) bool) ([]exchange.Order, error) {
	var out []exchange.Order
	err := waitForStage(ctx, env, stage, func() (bool, map[string]any, error) {
		orders, err := env.botClient.ListOpenOrders(ctx, env.spec.Symbol)
		if err != nil {
			return false, nil, err
		}
		out = orders
		return predicate(orders), map[string]any{
			"order_ids":  orderIDs(orders),
			"bid_count":  countSide(orders, exchange.SideBuy),
			"ask_count":  countSide(orders, exchange.SideSell),
			"orders_raw": orders,
		}, nil
	})
	return out, err
}

func waitForTrade(ctx context.Context, env *environment, stage string) error {
	return waitForStage(ctx, env, stage, func() (bool, map[string]any, error) {
		trades, err := env.botClient.GetTrades(ctx, env.spec.Symbol)
		if err != nil {
			return false, nil, err
		}
		return len(trades) > 0, map[string]any{"trade_count": len(trades)}, nil
	})
}

func waitForInventoryChange(ctx context.Context, env *environment, asset string, before float64, stage string) (float64, error) {
	var out float64
	err := waitForStage(ctx, env, stage, func() (bool, map[string]any, error) {
		value, err := readInventory(ctx, env.botClient, asset)
		if err != nil {
			return false, nil, err
		}
		out = value
		return value != before, map[string]any{"before": before, "current": value}, nil
	})
	return out, err
}

func submitTakerCross(ctx context.Context, env *environment, price, size float64, label string) error {
	env.logger.Info("phase", "name", label, "price", price, "size", size)
	nonce := strconv.FormatInt(time.Now().UnixMicro(), 10)
	orderID := fmt.Sprintf("integration-taker:%s:%s:%s", env.intCfg.Scenario, env.spec.Symbol, nonce)
	_, err := env.takerClient.PlaceLimitOrder(ctx, exchange.PlaceOrderRequest{
		Market:  env.spec.Symbol,
		Side:    exchange.SideBuy,
		Price:   price,
		Size:    size,
		OrderID: orderID,
		Nonce:   nonce,
	})
	return err
}

func readInventory(ctx context.Context, client exchange.Client, asset string) (float64, error) {
	balances, err := client.GetBalances(ctx)
	if err != nil {
		return 0, err
	}
	for _, balance := range balances {
		if balance.Asset == asset {
			return balance.Total, nil
		}
	}
	return 0, fmt.Errorf("asset %s not found in balances", asset)
}

func splitSides(orders []exchange.Order) (exchange.Order, exchange.Order) {
	var bid exchange.Order
	var ask exchange.Order
	for _, order := range orders {
		switch order.Side {
		case exchange.SideBuy:
			bid = order
		case exchange.SideSell:
			ask = order
		}
	}
	return bid, ask
}

func countSide(orders []exchange.Order, side exchange.Side) int {
	count := 0
	for _, order := range orders {
		if order.Side == side {
			count++
		}
	}
	return count
}

func noDuplicateSideOrders(orders []exchange.Order) bool {
	return countSide(orders, exchange.SideBuy) <= 1 && countSide(orders, exchange.SideSell) <= 1
}

func sameOrderSet(left, right []exchange.Order) bool {
	if len(left) != len(right) {
		return false
	}
	leftIDs := orderIDs(left)
	rightIDs := orderIDs(right)
	if len(leftIDs) != len(rightIDs) {
		return false
	}
	for i := range leftIDs {
		if leftIDs[i] != rightIDs[i] {
			return false
		}
	}
	return true
}

func orderIDs(orders []exchange.Order) []string {
	out := make([]string, 0, len(orders))
	for _, order := range orders {
		out = append(out, order.ID)
	}
	return out
}

func nonEmpty(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func waitForStage(ctx context.Context, env *environment, stage string, fn func() (bool, map[string]any, error)) error {
	env.logger.Info("phase", "name", stage, "scenario", env.intCfg.Scenario)
	ticker := time.NewTicker(env.intCfg.PollInterval)
	defer ticker.Stop()
	var lastDetail map[string]any
	for {
		done, detail, err := fn()
		if detail != nil {
			lastDetail = detail
		}
		if err != nil {
			return fmt.Errorf("%s failed: %w", stage, err)
		}
		if done {
			env.logger.Info("phase_complete", "name", stage, "detail", lastDetail)
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s timed out: %w detail=%v", stage, ctx.Err(), lastDetail)
		case <-ticker.C:
		}
	}
}

func stringsTrim(key string) string {
	return defaultEnv(key, "")
}

func defaultEnv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		panic(fmt.Sprintf("%s must be an integer: %v", key, err))
	}
	return value
}
