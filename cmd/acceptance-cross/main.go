package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/numofx/market-maker/internal/exchange"
)

func mustEnv(key string) string {
	value := os.Getenv(key)
	if value == "" {
		panic("missing env " + key)
	}
	return value
}

func newClient(ctx context.Context, subaccountID string) *exchange.HTTPClient {
	client, err := exchange.NewHTTPClient(ctx, exchange.ClientConfig{
		APIBaseURL:         mustEnv("MM_API_BASE_URL"),
		RPCURL:             mustEnv("MM_RPC_URL"),
		DatabaseURL:        mustEnv("MM_DATABASE_URL"),
		MarketSymbol:       "USDCcNGN-APR30-2026",
		ChainID:            84532,
		MatchingAddress:    mustEnv("MM_MATCHING_ADDRESS"),
		TradeModuleAddress: mustEnv("MM_TRADE_MODULE_ADDRESS"),
		SubAccountsAddress: mustEnv("MM_SUBACCOUNTS_ADDRESS"),
		OwnerAddress:       mustEnv("MM_OWNER_ADDRESS"),
		SignerAddress:      mustEnv("MM_SIGNER_ADDRESS"),
		OwnerPrivateKey:    mustEnv("MM_OWNER_PRIVATE_KEY"),
		SignerPrivateKey:   mustEnv("MM_SIGNER_PRIVATE_KEY"),
		SubaccountID:       subaccountID,
		RecipientID:        subaccountID,
		WorstFee:           "0",
		OrderExpirySeconds: 3600,
		ServiceName:        "validation-runner",
		ProtectedPrefixes:  []string{"validation:", "test:"},
	})
	if err != nil {
		panic(err)
	}
	return client
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	askClient := newClient(ctx, mustEnv("ASK_SUBACCOUNT_ID"))
	defer askClient.Close()
	buyClient := newClient(ctx, mustEnv("BUY_SUBACCOUNT_ID"))
	defer buyClient.Close()

	market := "USDCcNGN-APR30-2026"
	beforeTrades, err := askClient.GetTrades(ctx, market)
	if err != nil {
		panic(fmt.Sprintf("load trades before submit: %v", err))
	}

	nonceBase := time.Now().UnixMicro()
	askOrderID := fmt.Sprintf("validation:apr:ask:%d", nonceBase)
	buyOrderID := fmt.Sprintf("validation:apr:buy:%d", nonceBase)

	_, err = askClient.PlaceLimitOrder(ctx, exchange.PlaceOrderRequest{
		Market:  market,
		Side:    exchange.SideSell,
		Price:   1390,
		Size:    0.001,
		OrderID: askOrderID,
		Nonce:   fmt.Sprintf("%d", nonceBase),
	})
	if err != nil {
		panic(fmt.Sprintf("submit ask failed: %v", err))
	}

	_, err = buyClient.PlaceLimitOrder(ctx, exchange.PlaceOrderRequest{
		Market:  market,
		Side:    exchange.SideBuy,
		Price:   1391,
		Size:    0.001,
		OrderID: buyOrderID,
		Nonce:   fmt.Sprintf("%d", nonceBase+1),
	})
	if err != nil {
		panic(fmt.Sprintf("submit buy failed: %v", err))
	}

	afterCount := len(beforeTrades)
	tradeIncremented := false
	for i := 0; i < 20; i++ {
		time.Sleep(1500 * time.Millisecond)
		trades, pollErr := askClient.GetTrades(ctx, market)
		if pollErr != nil {
			continue
		}
		afterCount = len(trades)
		if afterCount > len(beforeTrades) {
			tradeIncremented = true
			break
		}
	}

	fmt.Printf("ASK_ORDER_ID=%s\n", askOrderID)
	fmt.Printf("BUY_ORDER_ID=%s\n", buyOrderID)
	fmt.Printf("TRADES_BEFORE=%d\n", len(beforeTrades))
	fmt.Printf("TRADES_AFTER=%d\n", afterCount)
	fmt.Printf("TRADE_INCREMENTED=%t\n", tradeIncremented)

	if !tradeIncremented {
		panic("trade count did not increase")
	}
}
