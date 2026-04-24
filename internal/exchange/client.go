package exchange

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	gethmath "github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Side string

const (
	SideBuy  Side = "buy"
	SideSell Side = "sell"
)

const assetDecimals = 18

var decimalScale = new(big.Int).Exp(big.NewInt(10), big.NewInt(assetDecimals), nil)

type BookLevel struct {
	Price float64 `json:"price"`
	Size  float64 `json:"size"`
}

type Book struct {
	Bids []BookLevel
	Asks []BookLevel
}

type Trade struct {
	ID        int64     `json:"id"`
	Price     float64   `json:"price"`
	Size      float64   `json:"size"`
	Side      Side      `json:"side"`
	CreatedAt time.Time `json:"created_at"`
}

type Balance struct {
	Asset     string  `json:"asset"`
	Available float64 `json:"available"`
	Reserved  float64 `json:"reserved"`
	Total     float64 `json:"total"`
}

type RawBalance struct {
	Asset       string
	SubID       string
	RawBalance  string
	HumanAmount float64
}

type Order struct {
	ID         string    `json:"id"`
	Market     string    `json:"market"`
	Side       Side      `json:"side"`
	Price      float64   `json:"price"`
	Size       float64   `json:"size"`
	RawSize    string    `json:"raw_size"`
	Nonce      string    `json:"nonce"`
	Owner      string    `json:"owner"`
	CreatedAt  time.Time `json:"created_at"`
	Managed    bool      `json:"managed"`
	Subaccount string    `json:"subaccount_id"`
}

type MarketSpec struct {
	Symbol         string
	BaseAsset      string
	QuoteAsset     string
	AssetAddress   string
	QuoteAddress   string
	SubID          string
	TickSize       float64
	SizeStep       float64
	MinSize        float64
	OrderEntrySpec string
}

type AssetCodeCheck struct {
	MarketSymbol string
	EnvVar       string
	Role         string
	Address      string
	RPCLabel     string
	HasCode      bool
	CodeBytes    int
}

type spotUIPresentation struct {
	UIIntent struct {
		Side  string `json:"side"`
		Price string `json:"price"`
		Size  string `json:"size"`
	} `json:"ui_intent"`
}

type Client interface {
	GetBook(ctx context.Context, market string) (Book, error)
	GetTrades(ctx context.Context, market string) ([]Trade, error)
	GetBalances(ctx context.Context) ([]Balance, error)
	ListOpenOrders(ctx context.Context, market string) ([]Order, error)
	PlaceLimitOrder(ctx context.Context, req PlaceOrderRequest) (Order, error)
	CancelOrder(ctx context.Context, orderID string, reason string) error
	CancelAllOrders(ctx context.Context, market string, reason string) error
	GetMarket(ctx context.Context, market string) (MarketSpec, error)
}

type ClientConfig struct {
	APIBaseURL         string
	RPCURL             string
	DatabaseURL        string
	MarketSymbol       string
	ChainID            int64
	MatchingRepoPath   string
	RiskCoreRepoPath   string
	MatchingAddress    string
	TradeModuleAddress string
	SubAccountsAddress string
	OwnerAddress       string
	SignerAddress      string
	OwnerPrivateKey    string
	SignerPrivateKey   string
	SubaccountID       string
	RecipientID        string
	WorstFee           string
	OrderExpirySeconds int64
	ServiceName        string
	ProtectedPrefixes  []string
}

type PlaceOrderRequest struct {
	Market  string
	Side    Side
	Price   float64
	Size    float64
	OrderID string
	Nonce   string
}

type HTTPClient struct {
	cfg         ClientConfig
	httpClient  *http.Client
	pg          *pgxpool.Pool
	rpc         *ethclient.Client
	ownerKey    *ecdsa.PrivateKey
	signerKey   *ecdsa.PrivateKey
	matching    common.Address
	tradeModule common.Address
	subAccounts common.Address
	quoteAsset  common.Address
	markets     map[string]MarketSpec
}

func NewHTTPClient(ctx context.Context, cfg ClientConfig) (*HTTPClient, error) {
	if cfg.APIBaseURL == "" {
		return nil, fmt.Errorf("APIBaseURL is required")
	}
	if cfg.RPCURL == "" {
		return nil, fmt.Errorf("RPCURL is required")
	}
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("DatabaseURL is required")
	}
	if cfg.SubaccountID == "" {
		return nil, fmt.Errorf("SubaccountID is required")
	}
	if cfg.OwnerPrivateKey == "" {
		return nil, fmt.Errorf("OwnerPrivateKey is required")
	}
	if cfg.SignerPrivateKey == "" {
		cfg.SignerPrivateKey = cfg.OwnerPrivateKey
	}
	if cfg.RecipientID == "" {
		cfg.RecipientID = cfg.SubaccountID
	}
	if cfg.WorstFee == "" {
		cfg.WorstFee = "0"
	}
	if cfg.OrderExpirySeconds <= 0 {
		cfg.OrderExpirySeconds = 3600
	}
	if cfg.MatchingRepoPath == "" {
		cfg.MatchingRepoPath = "../execution-contracts"
	}
	if cfg.RiskCoreRepoPath == "" {
		cfg.RiskCoreRepoPath = "../risk-core"
	}

	ownerKey, err := crypto.HexToECDSA(strings.TrimPrefix(cfg.OwnerPrivateKey, "0x"))
	if err != nil {
		return nil, fmt.Errorf("parse owner private key: %w", err)
	}
	signerKey, err := crypto.HexToECDSA(strings.TrimPrefix(cfg.SignerPrivateKey, "0x"))
	if err != nil {
		return nil, fmt.Errorf("parse signer private key: %w", err)
	}

	if cfg.OwnerAddress == "" {
		cfg.OwnerAddress = crypto.PubkeyToAddress(ownerKey.PublicKey).Hex()
	}
	if cfg.SignerAddress == "" {
		cfg.SignerAddress = crypto.PubkeyToAddress(signerKey.PublicKey).Hex()
	}
	cfg.OwnerAddress = strings.ToLower(cfg.OwnerAddress)
	cfg.SignerAddress = strings.ToLower(cfg.SignerAddress)

	if cfg.MatchingAddress == "" || cfg.TradeModuleAddress == "" {
		matchingAddress, tradeModuleAddress, err := loadMatchingDeployment(cfg.MatchingRepoPath, cfg.ChainID)
		if err != nil {
			return nil, err
		}
		if cfg.MatchingAddress == "" {
			cfg.MatchingAddress = matchingAddress.Hex()
		}
		if cfg.TradeModuleAddress == "" {
			cfg.TradeModuleAddress = tradeModuleAddress.Hex()
		}
	}
	if cfg.SubAccountsAddress == "" {
		address, err := loadSubAccountsDeployment(cfg.RiskCoreRepoPath, cfg.ChainID)
		if err != nil {
			return nil, err
		}
		cfg.SubAccountsAddress = address.Hex()
	}

	pg, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	rpc, err := ethclient.DialContext(ctx, cfg.RPCURL)
	if err != nil {
		return nil, fmt.Errorf("connect rpc: %w", err)
	}

	client := &HTTPClient{
		cfg:         cfg,
		httpClient:  &http.Client{Timeout: 15 * time.Second},
		pg:          pg,
		rpc:         rpc,
		ownerKey:    ownerKey,
		signerKey:   signerKey,
		matching:    common.HexToAddress(cfg.MatchingAddress),
		tradeModule: common.HexToAddress(cfg.TradeModuleAddress),
		subAccounts: common.HexToAddress(cfg.SubAccountsAddress),
		markets:     make(map[string]MarketSpec),
	}

	quoteAsset, err := client.readAddressCall(ctx, client.tradeModule, "quoteAsset", `{"name":"quoteAsset","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"address"}]}`)
	if err != nil {
		return nil, fmt.Errorf("read quoteAsset: %w", err)
	}
	client.quoteAsset = quoteAsset
	if err := client.loadMarkets(ctx); err != nil {
		return nil, err
	}
	return client, nil
}

func (c *HTTPClient) Close() {
	if c.pg != nil {
		c.pg.Close()
	}
	if c.rpc != nil {
		c.rpc.Close()
	}
}

func (c *HTTPClient) GetMarket(ctx context.Context, market string) (MarketSpec, error) {
	if spec, ok := c.markets[market]; ok {
		return spec, nil
	}
	if err := c.loadMarkets(ctx); err != nil {
		return MarketSpec{}, err
	}
	spec, ok := c.markets[market]
	if !ok {
		return MarketSpec{}, fmt.Errorf("unknown market %s", market)
	}
	return spec, nil
}

func (c *HTTPClient) ValidateMarketAssets(ctx context.Context, spec MarketSpec) ([]AssetCodeCheck, error) {
	checks := []AssetCodeCheck{
		{
			MarketSymbol: spec.Symbol,
			EnvVar:       assetAddressEnvVar(spec),
			Role:         "base_asset",
			Address:      spec.AssetAddress,
			RPCLabel:     c.cfg.RPCURL,
		},
		{
			MarketSymbol: spec.Symbol,
			EnvVar:       "TRADE_MODULE_QUOTE_ASSET",
			Role:         "quote_asset",
			Address:      spec.QuoteAddress,
			RPCLabel:     c.cfg.RPCURL,
		},
	}
	for i := range checks {
		address := common.HexToAddress(checks[i].Address)
		code, err := c.rpc.CodeAt(ctx, address, nil)
		if err != nil {
			return checks, fmt.Errorf("check %s code at %s: %w", checks[i].Role, checks[i].Address, err)
		}
		checks[i].HasCode = len(code) > 0
		checks[i].CodeBytes = len(code)
		attrs := []any{
			"market", checks[i].MarketSymbol,
			"role", checks[i].Role,
			"env_var", checks[i].EnvVar,
			"address", checks[i].Address,
			"rpc_label", checks[i].RPCLabel,
			"has_code", checks[i].HasCode,
			"code_bytes", checks[i].CodeBytes,
		}
		if checks[i].HasCode {
			slog.Info("market token code check passed", attrs...)
			continue
		}
		slog.Error("market token address has no code", attrs...)
	}
	if err := assetCodeReadinessError(checks); err != nil {
		return checks, err
	}
	return checks, nil
}

func assetCodeReadinessError(checks []AssetCodeCheck) error {
	var missing []string
	for _, check := range checks {
		if !check.HasCode {
			missing = append(missing, fmt.Sprintf("%s=%s", check.EnvVar, check.Address))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("selected market token_address_has_no_code: %s", strings.Join(missing, ", "))
}

func assetAddressEnvVar(spec MarketSpec) string {
	if spec.Symbol == "USDCcNGN-SPOT" {
		return "CNGN_SPOT_ASSET_ADDRESS"
	}
	return "MARKET_ASSET_ADDRESS"
}

func (c *HTTPClient) GetBook(ctx context.Context, market string) (Book, error) {
	spec, err := c.GetMarket(ctx, market)
	if err != nil {
		return Book{}, err
	}
	var resp struct {
		Bids []struct {
			LimitPrice    string              `json:"limit_price"`
			DesiredAmount string              `json:"desired_amount"`
			SpotContract  *spotUIPresentation `json:"spot_contract"`
		} `json:"bids"`
		Asks []struct {
			LimitPrice    string              `json:"limit_price"`
			DesiredAmount string              `json:"desired_amount"`
			SpotContract  *spotUIPresentation `json:"spot_contract"`
		} `json:"asks"`
	}
	if err := c.get(ctx, "/v1/book", url.Values{"symbol": []string{market}}, &resp); err != nil {
		return Book{}, err
	}
	book := Book{
		Bids: make([]BookLevel, 0, len(resp.Bids)),
		Asks: make([]BookLevel, 0, len(resp.Asks)),
	}
	for _, bid := range resp.Bids {
		level, side, err := parsePresentedBookLevel(spec, bid.LimitPrice, bid.DesiredAmount, bid.SpotContract, SideBuy)
		if err != nil {
			return Book{}, err
		}
		switch side {
		case SideBuy:
			book.Bids = append(book.Bids, level)
		case SideSell:
			book.Asks = append(book.Asks, level)
		}
	}
	for _, ask := range resp.Asks {
		level, side, err := parsePresentedBookLevel(spec, ask.LimitPrice, ask.DesiredAmount, ask.SpotContract, SideSell)
		if err != nil {
			return Book{}, err
		}
		switch side {
		case SideBuy:
			book.Bids = append(book.Bids, level)
		case SideSell:
			book.Asks = append(book.Asks, level)
		}
	}
	return book, nil
}

func (c *HTTPClient) GetTrades(ctx context.Context, market string) ([]Trade, error) {
	var resp struct {
		Trades []struct {
			TradeID       int64  `json:"trade_id"`
			Price         string `json:"price"`
			Size          string `json:"size"`
			AggressorSide Side   `json:"aggressor_side"`
			CreatedAt     string `json:"created_at"`
		} `json:"trades"`
	}
	if err := c.get(ctx, "/v1/trades", url.Values{"symbol": []string{market}, "limit": []string{"20"}}, &resp); err != nil {
		return nil, err
	}
	out := make([]Trade, 0, len(resp.Trades))
	for _, item := range resp.Trades {
		price, err := strconv.ParseFloat(item.Price, 64)
		if err != nil {
			return nil, fmt.Errorf("parse trade price: %w", err)
		}
		tm, _ := time.Parse(time.RFC3339Nano, item.CreatedAt)
		out = append(out, Trade{
			ID:        item.TradeID,
			Price:     price,
			Size:      rawToFloat(item.Size),
			Side:      item.AggressorSide,
			CreatedAt: tm,
		})
	}
	return out, nil
}

func (c *HTTPClient) GetBalances(ctx context.Context) ([]Balance, error) {
	positions, rawBalances, err := c.readAccountBalances(ctx)
	if err != nil {
		return nil, err
	}

	exposures := make(map[string]float64)
	rows, err := c.pg.Query(ctx, `
select side, desired_amount, limit_price, asset_address, sub_id
from active_orders
where owner_address = $1 and subaccount_id = $2 and status = 'active'
`, c.cfg.OwnerAddress, c.cfg.SubaccountID)
	if err != nil {
		return nil, fmt.Errorf("query exposures: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var side string
		var rawSize string
		var price string
		var assetAddress string
		var subID string
		if err := rows.Scan(&side, &rawSize, &price, &assetAddress, &subID); err != nil {
			return nil, fmt.Errorf("scan exposure: %w", err)
		}
		size := rawToFloat(rawSize)
		px, err := strconv.ParseFloat(price, 64)
		if err != nil {
			return nil, fmt.Errorf("parse exposure price: %w", err)
		}
		assetKey, reserved := c.reservedExposureKey(side, size, px, assetAddress, subID)
		exposures[assetKey] += reserved
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	spec, err := c.marketForBalances()
	if err != nil {
		return nil, err
	}
	baseKey, quoteKey := balanceKeys(spec)
	for _, row := range rawBalances {
		role := "unmapped"
		key := strings.ToLower(row.Asset) + "|" + row.SubID
		switch key {
		case baseKey:
			role = "base"
		case quoteKey:
			role = "quote"
		}
		slog.Info("subaccount balance observed", "market", spec.Symbol, "subaccount_id", c.cfg.SubaccountID, "asset_address", row.Asset, "sub_id", row.SubID, "raw_balance", row.RawBalance, "amount", row.HumanAmount, "role", role)
	}
	out := make([]Balance, 0, 2)
	baseTotal := positions[baseKey]
	baseReserved := exposures[baseKey]
	baseAvailable := maxFloat(0, baseTotal-baseReserved)
	slog.Info("subaccount balance mapped", "market", spec.Symbol, "subaccount_id", c.cfg.SubaccountID, "asset", spec.BaseAsset, "asset_address", strings.Split(baseKey, "|")[0], "sub_id", strings.Split(baseKey, "|")[1], "total", baseTotal, "reserved", baseReserved, "available", baseAvailable)
	out = append(out, Balance{
		Asset:     spec.BaseAsset,
		Total:     baseTotal,
		Reserved:  baseReserved,
		Available: baseAvailable,
	})

	quoteTotal := positions[quoteKey]
	quoteReserved := exposures[quoteKey]
	quoteAvailable := maxFloat(0, quoteTotal-quoteReserved)
	slog.Info("subaccount balance mapped", "market", spec.Symbol, "subaccount_id", c.cfg.SubaccountID, "asset", spec.QuoteAsset, "asset_address", strings.Split(quoteKey, "|")[0], "sub_id", strings.Split(quoteKey, "|")[1], "total", quoteTotal, "reserved", quoteReserved, "available", quoteAvailable)
	out = append(out, Balance{
		Asset:     spec.QuoteAsset,
		Total:     quoteTotal,
		Reserved:  quoteReserved,
		Available: quoteAvailable,
	})
	return dedupeBalances(out), nil
}

func (c *HTTPClient) ListOpenOrders(ctx context.Context, market string) ([]Order, error) {
	spec, err := c.GetMarket(ctx, market)
	if err != nil {
		return nil, err
	}

	rows, err := c.pg.Query(ctx, `
select order_id, side, limit_price, desired_amount, filled_amount, nonce, owner_address, created_at, subaccount_id
from active_orders
where owner_address = $1 and asset_address = $2 and sub_id = $3 and status = 'active'
order by created_at asc
`, c.cfg.OwnerAddress, strings.ToLower(spec.AssetAddress), spec.SubID)
	if err != nil {
		return nil, fmt.Errorf("query open orders: %w", err)
	}
	defer rows.Close()

	var out []Order
	for rows.Next() {
		var (
			orderID       string
			side          string
			limitPrice    string
			desiredAmount string
			filledAmount  string
			nonce         string
			owner         string
			createdAt     time.Time
			subaccountID  string
		)
		if err := rows.Scan(&orderID, &side, &limitPrice, &desiredAmount, &filledAmount, &nonce, &owner, &createdAt, &subaccountID); err != nil {
			return nil, fmt.Errorf("scan order: %w", err)
		}
		price, err := strconv.ParseFloat(limitPrice, 64)
		if err != nil {
			return nil, fmt.Errorf("parse order price: %w", err)
		}
		remainingRaw, err := subtractRaw(desiredAmount, filledAmount)
		if err != nil {
			return nil, fmt.Errorf("compute remaining size: %w", err)
		}
		sideValue := Side(side)
		size := rawToFloat(remainingRaw)
		if spec.Symbol == "USDCcNGN-SPOT" {
			sideValue, price, size, err = spotUIFromEngine(sideValue, price, size)
			if err != nil {
				return nil, fmt.Errorf("decode spot open order: %w", err)
			}
		}
		out = append(out, Order{
			ID:         orderID,
			Market:     market,
			Side:       sideValue,
			Price:      price,
			Size:       size,
			RawSize:    remainingRaw,
			Nonce:      nonce,
			Owner:      owner,
			CreatedAt:  createdAt,
			Managed:    strings.HasPrefix(orderID, managedOrderPrefix(market)),
			Subaccount: subaccountID,
		})
	}
	return out, rows.Err()
}

func (c *HTTPClient) PlaceLimitOrder(ctx context.Context, req PlaceOrderRequest) (Order, error) {
	spec, err := c.GetMarket(ctx, req.Market)
	if err != nil {
		return Order{}, err
	}
	if req.Side != SideBuy && req.Side != SideSell {
		return Order{}, fmt.Errorf("invalid side %q", req.Side)
	}

	engineSide := req.Side
	enginePrice := req.Price
	engineAmount := req.Size
	payloadSide := req.Side
	payloadDesiredAmount := floatToRaw(req.Size)
	payloadLimitPrice := normalizePrice(req.Price)
	payload := map[string]any{}
	if spec.Symbol == "USDCcNGN-SPOT" {
		engineSide, enginePrice, engineAmount, err = spotEngineFromUI(req.Side, req.Price, req.Size)
		if err != nil {
			return Order{}, fmt.Errorf("translate spot order: %w", err)
		}
		payloadSide = engineSide
		payloadDesiredAmount = floatToRaw(engineAmount)
		payloadLimitPrice = normalizePrice(enginePrice)
		payload["order_entry_spec"] = "usdc_cngn_spot_v1"
		payload["ui_intent"] = map[string]string{
			"side":  string(req.Side),
			"price": normalizePrice(req.Price),
			"size":  normalizePrice(req.Size),
		}
	}
	actionData, err := encodeTradeData(spec.AssetAddress, spec.SubID, enginePrice, payloadDesiredAmount, c.cfg.RecipientID, engineSide == SideBuy, c.cfg.WorstFee)
	if err != nil {
		return Order{}, err
	}
	expiry := time.Now().UTC().Add(time.Duration(c.cfg.OrderExpirySeconds) * time.Second).Unix()
	actionJSON := map[string]string{
		"subaccount_id": c.cfg.SubaccountID,
		"nonce":         req.Nonce,
		"module":        c.tradeModule.Hex(),
		"data":          actionData,
		"expiry":        strconv.FormatInt(expiry, 10),
		"owner":         c.cfg.OwnerAddress,
		"signer":        c.cfg.SignerAddress,
	}
	signature, err := c.signAction(actionJSON)
	if err != nil {
		return Order{}, err
	}

	payload["order_id"] = req.OrderID
	payload["owner_address"] = c.cfg.OwnerAddress
	payload["signer_address"] = c.cfg.SignerAddress
	payload["subaccount_id"] = c.cfg.SubaccountID
	payload["recipient_id"] = c.cfg.RecipientID
	payload["nonce"] = req.Nonce
	payload["asset_address"] = spec.AssetAddress
	payload["sub_id"] = spec.SubID
	payload["filled_amount"] = "0"
	payload["worst_fee"] = c.cfg.WorstFee
	payload["expiry"] = expiry
	payload["action_json"] = actionJSON
	payload["signature"] = signature
	if spec.Symbol == "USDCcNGN-SPOT" {
		payload["side"] = ""
		payload["desired_amount"] = ""
		payload["limit_price"] = ""
	} else {
		payload["side"] = payloadSide
		payload["desired_amount"] = payloadDesiredAmount
		payload["limit_price"] = payloadLimitPrice
	}
	var resp struct {
		Order struct {
			OrderID       string `json:"order_id"`
			Nonce         string `json:"nonce"`
			OwnerAddress  string `json:"owner_address"`
			SubaccountID  string `json:"subaccount_id"`
			Side          Side   `json:"side"`
			LimitPrice    string `json:"limit_price"`
			DesiredAmount string `json:"desired_amount"`
			CreatedAt     string `json:"created_at"`
			Market        string `json:"market"`
		} `json:"order"`
	}
	if err := c.post(ctx, "/v1/orders", payload, &resp); err != nil {
		return Order{}, err
	}

	price, _ := strconv.ParseFloat(resp.Order.LimitPrice, 64)
	size := rawToFloat(resp.Order.DesiredAmount)
	side := resp.Order.Side
	if spec.Symbol == "USDCcNGN-SPOT" {
		side, price, size, err = spotUIFromEngine(resp.Order.Side, price, size)
		if err != nil {
			return Order{}, fmt.Errorf("decode spot placed order: %w", err)
		}
	}
	tm, _ := time.Parse(time.RFC3339Nano, resp.Order.CreatedAt)
	return Order{
		ID:         resp.Order.OrderID,
		Market:     req.Market,
		Side:       side,
		Price:      price,
		Size:       size,
		RawSize:    resp.Order.DesiredAmount,
		Nonce:      resp.Order.Nonce,
		Owner:      resp.Order.OwnerAddress,
		CreatedAt:  tm,
		Managed:    true,
		Subaccount: resp.Order.SubaccountID,
	}, nil
}

func (c *HTTPClient) CancelOrder(ctx context.Context, orderID string, reason string) error {
	if isProtectedOrderID(orderID, c.cfg.ProtectedPrefixes) {
		slog.Info("skip protected order cancel", "order_id", orderID, "reason", reason)
		return nil
	}
	orders, err := c.lookupOrderByID(ctx, orderID)
	if err != nil {
		return err
	}
	body := map[string]string{
		"owner_address": c.cfg.OwnerAddress,
		"nonce":         orders.Nonce,
		"reason":        machineCancelReason(c.cfg.ServiceName, reason),
		"service":       normalizeCancelToken(c.cfg.ServiceName),
	}
	return c.post(ctx, "/v1/orders/cancel", body, nil)
}

func (c *HTTPClient) CancelAllOrders(ctx context.Context, market string, reason string) error {
	orders, err := c.ListOpenOrders(ctx, market)
	if err != nil {
		return err
	}
	for _, order := range orders {
		if err := c.CancelOrder(ctx, order.ID, reason); err != nil && !strings.Contains(err.Error(), "active order not found") {
			return err
		}
	}
	return nil
}

func machineCancelReason(service string, reason string) string {
	serviceToken := normalizeCancelToken(service)
	if serviceToken == "" {
		serviceToken = "unknown_service"
	}
	reasonToken := normalizeCancelToken(reason)
	if reasonToken == "" {
		reasonToken = "unspecified"
	}
	return "bot." + serviceToken + "." + reasonToken
}

func normalizeCancelToken(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, " ", "_")
	if value == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(value))
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('_')
	}
	return strings.Trim(b.String(), "_.-")
}

func isProtectedOrderID(orderID string, prefixes []string) bool {
	orderID = strings.TrimSpace(orderID)
	if orderID == "" || len(prefixes) == 0 {
		return false
	}
	for _, prefix := range prefixes {
		prefix = strings.TrimSpace(prefix)
		if prefix == "" {
			continue
		}
		if strings.HasPrefix(orderID, prefix) {
			return true
		}
	}
	return false
}

func (c *HTTPClient) lookupOrderByID(ctx context.Context, orderID string) (Order, error) {
	row := c.pg.QueryRow(ctx, `
select order_id, side, limit_price, desired_amount, nonce, owner_address, created_at, subaccount_id
from active_orders
where order_id = $1 and owner_address = $2
`, orderID, c.cfg.OwnerAddress)
	var (
		id            string
		side          string
		limitPrice    string
		desiredAmount string
		nonce         string
		owner         string
		createdAt     time.Time
		subaccountID  string
	)
	if err := row.Scan(&id, &side, &limitPrice, &desiredAmount, &nonce, &owner, &createdAt, &subaccountID); err != nil {
		return Order{}, fmt.Errorf("lookup order %s: %w", orderID, err)
	}
	price, _ := strconv.ParseFloat(limitPrice, 64)
	size := rawToFloat(desiredAmount)
	sideValue := Side(side)
	return Order{
		ID:         id,
		Side:       sideValue,
		Price:      price,
		Size:       size,
		RawSize:    desiredAmount,
		Nonce:      nonce,
		Owner:      owner,
		CreatedAt:  createdAt,
		Subaccount: subaccountID,
	}, nil
}

func (c *HTTPClient) loadMarkets(ctx context.Context) error {
	var resp []struct {
		Market           string `json:"market"`
		BaseAssetSymbol  string `json:"base_asset_symbol"`
		QuoteAssetSymbol string `json:"quote_asset_symbol"`
		AssetAddress     string `json:"asset_address"`
		SubID            string `json:"sub_id"`
		TickSize         string `json:"tick_size"`
		OrderEntrySpec   string `json:"order_entry_spec"`
	}
	if err := c.get(ctx, "/v1/markets", nil, &resp); err != nil {
		return err
	}
	for _, item := range resp {
		tickSize, _ := strconv.ParseFloat(item.TickSize, 64)
		spec := MarketSpec{
			Symbol:         item.Market,
			BaseAsset:      item.BaseAssetSymbol,
			QuoteAsset:     item.QuoteAssetSymbol,
			AssetAddress:   strings.ToLower(item.AssetAddress),
			SubID:          defaultString(item.SubID, "0"),
			TickSize:       tickSize,
			QuoteAddress:   strings.ToLower(c.quoteAsset.Hex()),
			OrderEntrySpec: item.OrderEntrySpec,
		}
		switch item.Market {
		case "USDCcNGN-SPOT":
			spec.SizeStep = 0.000001
			spec.MinSize = 0.000001
		case "USDCcNGN-APR30-2026":
			spec.SizeStep = 0.001
			spec.MinSize = 0.001
		default:
			spec.SizeStep = 0.000001
			spec.MinSize = 0.000001
		}
		c.markets[item.Market] = spec
	}
	if len(c.markets) == 0 {
		return fmt.Errorf("no markets returned by exchange")
	}
	return nil
}

func (c *HTTPClient) marketForBalances() (MarketSpec, error) {
	if c.cfg.MarketSymbol != "" {
		spec, ok := c.markets[c.cfg.MarketSymbol]
		if !ok {
			return MarketSpec{}, fmt.Errorf("configured market %s not loaded", c.cfg.MarketSymbol)
		}
		return spec, nil
	}
	for _, spec := range c.markets {
		if spec.AssetAddress != "" && spec.QuoteAddress != "" {
			return spec, nil
		}
	}
	return MarketSpec{}, fmt.Errorf("no market available for balance mapping")
}

func (c *HTTPClient) readAccountBalances(ctx context.Context) (map[string]float64, []RawBalance, error) {
	subaccountID, ok := new(big.Int).SetString(c.cfg.SubaccountID, 10)
	if !ok {
		return nil, nil, fmt.Errorf("invalid subaccount id %q", c.cfg.SubaccountID)
	}
	callABI, err := abi.JSON(strings.NewReader(`[
{"name":"getAccountBalances","type":"function","stateMutability":"view","inputs":[{"name":"accountId","type":"uint256"}],"outputs":[{"name":"assetBalances","type":"tuple[]","components":[{"name":"asset","type":"address"},{"name":"subId","type":"uint256"},{"name":"balance","type":"int256"}]}]},
{"name":"quoteAsset","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"address"}]}
]`))
	if err != nil {
		return nil, nil, err
	}
	input, err := callABI.Pack("getAccountBalances", subaccountID)
	if err != nil {
		return nil, nil, err
	}
	output, err := c.rpc.CallContract(ctx, ethereumCallMsg(c.subAccounts, input), nil)
	if err != nil {
		return nil, nil, fmt.Errorf("getAccountBalances rpc: %w", err)
	}
	values, err := callABI.Unpack("getAccountBalances", output)
	if err != nil {
		return nil, nil, fmt.Errorf("decode account balances: %w", err)
	}
	type balanceRow struct {
		Asset   common.Address `json:"asset"`
		SubId   *big.Int       `json:"subId"`
		Balance *big.Int       `json:"balance"`
	}
	items, ok := values[0].([]struct {
		Asset   common.Address `json:"asset"`
		SubId   *big.Int       `json:"subId"`
		Balance *big.Int       `json:"balance"`
	})
	positions := make(map[string]float64)
	raw := make([]RawBalance, 0)
	if ok {
		for _, item := range items {
			amount := rawBigToFloat(item.Balance)
			positions[strings.ToLower(item.Asset.Hex())+"|"+item.SubId.String()] = amount
			raw = append(raw, RawBalance{Asset: item.Asset.Hex(), SubID: item.SubId.String(), RawBalance: item.Balance.String(), HumanAmount: amount})
		}
		return positions, raw, nil
	}

	generic, ok := values[0].([]balanceRow)
	if !ok {
		return nil, nil, fmt.Errorf("unexpected balance payload %T", values[0])
	}
	for _, item := range generic {
		amount := rawBigToFloat(item.Balance)
		positions[strings.ToLower(item.Asset.Hex())+"|"+item.SubId.String()] = amount
		raw = append(raw, RawBalance{Asset: item.Asset.Hex(), SubID: item.SubId.String(), RawBalance: item.Balance.String(), HumanAmount: amount})
	}
	return positions, raw, nil
}

func (c *HTTPClient) readAddressCall(ctx context.Context, address common.Address, method string, abiJSON string) (common.Address, error) {
	parsed, err := abi.JSON(strings.NewReader("[" + abiJSON + "]"))
	if err != nil {
		return common.Address{}, err
	}
	input, err := parsed.Pack(method)
	if err != nil {
		return common.Address{}, err
	}
	output, err := c.rpc.CallContract(ctx, ethereumCallMsg(address, input), nil)
	if err != nil {
		c.logRPCCallFailure(ctx, address, method, input, nil, err)
		return common.Address{}, err
	}
	values, err := parsed.Unpack(method, output)
	if err != nil {
		c.logRPCCallFailure(ctx, address, method, input, output, err)
		return common.Address{}, err
	}
	if len(values) != 1 {
		c.logRPCCallFailure(ctx, address, method, input, output, fmt.Errorf("unexpected output count %d", len(values)))
		return common.Address{}, fmt.Errorf("unexpected output count")
	}
	addr, ok := values[0].(common.Address)
	if !ok {
		c.logRPCCallFailure(ctx, address, method, input, output, fmt.Errorf("unexpected address output %T", values[0]))
		return common.Address{}, fmt.Errorf("unexpected address output %T", values[0])
	}
	return addr, nil
}

func (c *HTTPClient) logRPCCallFailure(ctx context.Context, address common.Address, method string, input, output []byte, callErr error) {
	status, body, probeErr := c.rawRPCProbe(ctx, address, input)
	attrs := []any{
		"rpc_url", c.cfg.RPCURL,
		"http_method", http.MethodPost,
		"contract_address", address.Hex(),
		"abi_method", method,
		"call_data", hexutil.Encode(input),
		"ethclient_error", callErr.Error(),
		"probe_status", status,
		"probe_raw_body", body,
		"call_output", hexutil.Encode(output),
	}
	if probeErr != nil {
		attrs = append(attrs, "probe_error", probeErr.Error())
	}
	slog.Error("rpc call decode failure", attrs...)
}

func (c *HTTPClient) rawRPCProbe(ctx context.Context, to common.Address, data []byte) (int, string, error) {
	if !strings.HasPrefix(c.cfg.RPCURL, "http://") && !strings.HasPrefix(c.cfg.RPCURL, "https://") {
		return 0, "", fmt.Errorf("rpc url %q is not http(s)", c.cfg.RPCURL)
	}
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_call",
		"params": []any{
			map[string]string{
				"to":   to.Hex(),
				"data": hexutil.Encode(data),
			},
			"latest",
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.RPCURL, bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	return resp.StatusCode, string(raw), nil
}

func (c *HTTPClient) signAction(action map[string]string) (string, error) {
	td := apitypes.TypedData{
		Types: apitypes.Types{
			"EIP712Domain": {
				{Name: "name", Type: "string"},
				{Name: "version", Type: "string"},
				{Name: "chainId", Type: "uint256"},
				{Name: "verifyingContract", Type: "address"},
			},
			"Action": {
				{Name: "subaccountId", Type: "uint256"},
				{Name: "nonce", Type: "uint256"},
				{Name: "module", Type: "address"},
				{Name: "data", Type: "bytes"},
				{Name: "expiry", Type: "uint256"},
				{Name: "owner", Type: "address"},
				{Name: "signer", Type: "address"},
			},
		},
		PrimaryType: "Action",
		Domain: apitypes.TypedDataDomain{
			Name:              "Matching",
			Version:           "1.0",
			ChainId:           (*gethmath.HexOrDecimal256)(big.NewInt(c.cfg.ChainID)),
			VerifyingContract: c.matching.Hex(),
		},
		Message: apitypes.TypedDataMessage{
			"subaccountId": action["subaccount_id"],
			"nonce":        action["nonce"],
			"module":       action["module"],
			"data":         hexutil.MustDecode(action["data"]),
			"expiry":       action["expiry"],
			"owner":        action["owner"],
			"signer":       action["signer"],
		},
	}
	hash, _, err := apitypes.TypedDataAndHash(td)
	if err != nil {
		return "", fmt.Errorf("hash typed data: %w", err)
	}
	sig, err := crypto.Sign(hash, c.signerKey)
	if err != nil {
		return "", fmt.Errorf("sign typed data: %w", err)
	}
	sig[64] += 27
	return hexutil.Encode(sig), nil
}

func (c *HTTPClient) get(ctx context.Context, path string, query url.Values, out any) error {
	u := strings.TrimRight(c.cfg.APIBaseURL, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	return c.do(req, out)
}

func (c *HTTPClient) post(ctx context.Context, path string, payload any, out any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.cfg.APIBaseURL, "/")+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, out)
}

func (c *HTTPClient) do(req *http.Request, out any) error {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return fmt.Errorf("%s %s returned %d: %s", req.Method, req.URL.Path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func loadMatchingDeployment(repoPath string, chainID int64) (common.Address, common.Address, error) {
	path := filepath.Join(repoPath, "deployments", strconv.FormatInt(chainID, 10), "matching.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return common.Address{}, common.Address{}, fmt.Errorf("read matching deployment %s: %w", path, err)
	}
	var payload struct {
		Matching string `json:"matching"`
		Trade    string `json:"trade"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return common.Address{}, common.Address{}, err
	}
	if payload.Matching == "" || payload.Trade == "" {
		return common.Address{}, common.Address{}, fmt.Errorf("matching deployment missing matching/trade")
	}
	return common.HexToAddress(payload.Matching), common.HexToAddress(payload.Trade), nil
}

func loadSubAccountsDeployment(repoPath string, chainID int64) (common.Address, error) {
	path := filepath.Join(repoPath, "deployments", strconv.FormatInt(chainID, 10), "core.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return common.Address{}, fmt.Errorf("read core deployment %s: %w", path, err)
	}
	var payload struct {
		SubAccounts string `json:"subAccounts"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return common.Address{}, err
	}
	if payload.SubAccounts == "" {
		return common.Address{}, fmt.Errorf("core deployment missing subAccounts")
	}
	return common.HexToAddress(payload.SubAccounts), nil
}

func encodeTradeData(assetAddress, subID string, price float64, rawAmount string, recipientID string, isBid bool, worstFee string) (string, error) {
	types := abi.Arguments{
		{
			Type: mustTupleType([]abi.ArgumentMarshaling{
				{Name: "asset", Type: "address"},
				{Name: "subId", Type: "uint256"},
				{Name: "limitPrice", Type: "int256"},
				{Name: "desiredAmount", Type: "int256"},
				{Name: "worstFee", Type: "uint256"},
				{Name: "recipientId", Type: "uint256"},
				{Name: "isBid", Type: "bool"},
			}),
		},
	}

	priceRaw := floatToRaw(price)
	subIDInt, ok := new(big.Int).SetString(subID, 10)
	if !ok {
		return "", fmt.Errorf("invalid sub_id %q", subID)
	}
	amountInt, ok := new(big.Int).SetString(rawAmount, 10)
	if !ok {
		return "", fmt.Errorf("invalid raw amount %q", rawAmount)
	}
	priceInt, ok := new(big.Int).SetString(priceRaw, 10)
	if !ok {
		return "", fmt.Errorf("invalid raw price %q", priceRaw)
	}
	recipientInt, ok := new(big.Int).SetString(recipientID, 10)
	if !ok {
		return "", fmt.Errorf("invalid recipient id %q", recipientID)
	}
	worstFeeInt, ok := new(big.Int).SetString(worstFee, 10)
	if !ok {
		return "", fmt.Errorf("invalid worst fee %q", worstFee)
	}
	packed, err := types.Pack(struct {
		Asset         common.Address
		SubId         *big.Int
		LimitPrice    *big.Int
		DesiredAmount *big.Int
		WorstFee      *big.Int
		RecipientId   *big.Int
		IsBid         bool
	}{
		Asset:         common.HexToAddress(assetAddress),
		SubId:         subIDInt,
		LimitPrice:    priceInt,
		DesiredAmount: amountInt,
		WorstFee:      worstFeeInt,
		RecipientId:   recipientInt,
		IsBid:         isBid,
	})
	if err != nil {
		return "", fmt.Errorf("pack trade data: %w", err)
	}
	return hexutil.Encode(packed), nil
}

func mustTupleType(args []abi.ArgumentMarshaling) abi.Type {
	typ, err := abi.NewType("tuple", "", args)
	if err != nil {
		panic(err)
	}
	return typ
}

func rawToFloat(raw string) float64 {
	value, ok := new(big.Int).SetString(strings.TrimSpace(raw), 10)
	if !ok {
		f, _ := strconv.ParseFloat(raw, 64)
		return f
	}
	return rawBigToFloat(value)
}

func rawBigToFloat(value *big.Int) float64 {
	if value == nil {
		return 0
	}
	rat := new(big.Rat).SetFrac(value, decimalScale)
	out, _ := rat.Float64()
	return out
}

func floatToRaw(value float64) string {
	// Convert through a decimal string to avoid float64 binary drift
	// (e.g. 0.1 becoming 0.10000000000000000555...).
	normalized := normalizeDecimalString(strconv.FormatFloat(value, 'f', 18, 64))
	rat, ok := new(big.Rat).SetString(normalized)
	if !ok {
		rat = new(big.Rat).SetFloat64(value)
	}
	rat.Mul(rat, new(big.Rat).SetInt(decimalScale))
	out := new(big.Int)
	ratNum := new(big.Int).Set(rat.Num())
	ratDen := new(big.Int).Set(rat.Denom())
	out.Quo(ratNum, ratDen)
	return out.String()
}

func normalizePrice(price float64) string {
	return strconv.FormatFloat(price, 'f', -1, 64)
}

func normalizeDecimalString(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return raw
	}
	if !strings.Contains(raw, ".") {
		return raw
	}
	raw = strings.TrimRight(raw, "0")
	raw = strings.TrimRight(raw, ".")
	if raw == "" || raw == "-" {
		return "0"
	}
	return raw
}

func parsePresentedBookLevel(spec MarketSpec, rawPrice, rawAmount string, spot *spotUIPresentation, fallbackSide Side) (BookLevel, Side, error) {
	if spec.Symbol == "USDCcNGN-SPOT" && spot != nil {
		price, err := strconv.ParseFloat(spot.UIIntent.Price, 64)
		if err != nil {
			return BookLevel{}, "", fmt.Errorf("parse spot book ui price: %w", err)
		}
		size, err := strconv.ParseFloat(spot.UIIntent.Size, 64)
		if err != nil {
			return BookLevel{}, "", fmt.Errorf("parse spot book ui size: %w", err)
		}
		return BookLevel{Price: price, Size: size}, Side(strings.ToLower(spot.UIIntent.Side)), nil
	}
	price, err := strconv.ParseFloat(rawPrice, 64)
	if err != nil {
		return BookLevel{}, "", fmt.Errorf("parse book price: %w", err)
	}
	return BookLevel{Price: price, Size: rawToFloat(rawAmount)}, fallbackSide, nil
}

func balanceKeys(spec MarketSpec) (string, string) {
	if spec.Symbol == "USDCcNGN-SPOT" {
		return strings.ToLower(spec.QuoteAddress) + "|0", strings.ToLower(spec.AssetAddress) + "|" + spec.SubID
	}
	return strings.ToLower(spec.AssetAddress) + "|" + spec.SubID, strings.ToLower(spec.QuoteAddress) + "|0"
}

func (c *HTTPClient) reservedExposureKey(side string, size float64, px float64, assetAddress string, subID string) (string, float64) {
	for _, spec := range c.markets {
		if strings.ToLower(spec.AssetAddress) != strings.ToLower(assetAddress) || spec.SubID != subID {
			continue
		}
		if spec.Symbol == "USDCcNGN-SPOT" {
			if Side(side) == SideBuy {
				return strings.ToLower(spec.QuoteAddress) + "|0", size * px
			}
			return strings.ToLower(spec.AssetAddress) + "|" + spec.SubID, size
		}
		if Side(side) == SideBuy {
			return strings.ToLower(spec.QuoteAddress) + "|0", size * px
		}
		return strings.ToLower(spec.AssetAddress) + "|" + spec.SubID, size
	}
	if Side(side) == SideBuy {
		return strings.ToLower(assetAddress) + "|" + subID, size * px
	}
	return strings.ToLower(assetAddress) + "|" + subID, size
}

func spotEngineFromUI(uiSide Side, uiPrice float64, uiSize float64) (Side, float64, float64, error) {
	if uiPrice <= 0 || uiSize <= 0 {
		return "", 0, 0, fmt.Errorf("spot ui price and size must be positive")
	}
	enginePrice := 1 / uiPrice
	engineAmount := uiSize * uiPrice
	engineSide := SideBuy
	if uiSide == SideBuy {
		engineSide = SideSell
	}
	return engineSide, enginePrice, engineAmount, nil
}

func spotUIFromEngine(engineSide Side, enginePrice float64, engineAmount float64) (Side, float64, float64, error) {
	if enginePrice <= 0 || engineAmount <= 0 {
		return "", 0, 0, fmt.Errorf("spot engine price and amount must be positive")
	}
	uiSide := SideSell
	if engineSide == SideSell {
		uiSide = SideBuy
	}
	return uiSide, 1 / enginePrice, engineAmount * enginePrice, nil
}

func subtractRaw(left, right string) (string, error) {
	leftInt, ok := new(big.Int).SetString(strings.TrimSpace(left), 10)
	if ok {
		rightInt, ok := new(big.Int).SetString(strings.TrimSpace(right), 10)
		if !ok {
			return "", fmt.Errorf("invalid raw decimal %q", right)
		}
		result := new(big.Int).Sub(leftInt, rightInt)
		if result.Sign() < 0 {
			return "", fmt.Errorf("negative remaining amount")
		}
		return result.String(), nil
	}

	leftRat, ok := new(big.Rat).SetString(strings.TrimSpace(left))
	if !ok {
		return "", fmt.Errorf("invalid raw decimal %q", left)
	}
	rightRat, ok := new(big.Rat).SetString(strings.TrimSpace(right))
	if !ok {
		return "", fmt.Errorf("invalid raw decimal %q", right)
	}
	result := new(big.Rat).Sub(leftRat, rightRat)
	if result.Sign() < 0 {
		return "", fmt.Errorf("negative remaining amount")
	}
	return normalizeDecimalString(result.FloatString(18)), nil
}

func dedupeBalances(items []Balance) []Balance {
	seen := make(map[string]Balance)
	for _, item := range items {
		if item.Asset == "" {
			continue
		}
		seen[item.Asset] = item
	}
	out := make([]Balance, 0, len(seen))
	for _, item := range seen {
		out = append(out, item)
	}
	return out
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func managedOrderPrefix(market string) string {
	return "mm:" + market + ":"
}

func ethereumCallMsg(to common.Address, data []byte) ethereum.CallMsg {
	return ethereum.CallMsg{To: &to, Data: data}
}
