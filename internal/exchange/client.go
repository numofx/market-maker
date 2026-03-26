package exchange

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"log/slog"
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
	Symbol       string
	BaseAsset    string
	QuoteAsset   string
	AssetAddress string
	QuoteAddress string
	SubID        string
	TickSize     float64
	SizeStep     float64
	MinSize      float64
}

type Client interface {
	GetBook(ctx context.Context, market string) (Book, error)
	GetTrades(ctx context.Context, market string) ([]Trade, error)
	GetBalances(ctx context.Context) ([]Balance, error)
	ListOpenOrders(ctx context.Context, market string) ([]Order, error)
	PlaceLimitOrder(ctx context.Context, req PlaceOrderRequest) (Order, error)
	CancelOrder(ctx context.Context, orderID string) error
	CancelAllOrders(ctx context.Context, market string) error
	GetMarket(ctx context.Context, market string) (MarketSpec, error)
}

type ClientConfig struct {
	APIBaseURL         string
	RPCURL             string
	DatabaseURL        string
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

func (c *HTTPClient) GetBook(ctx context.Context, market string) (Book, error) {
	var resp struct {
		Bids []struct {
			LimitPrice    string `json:"limit_price"`
			DesiredAmount string `json:"desired_amount"`
		} `json:"bids"`
		Asks []struct {
			LimitPrice    string `json:"limit_price"`
			DesiredAmount string `json:"desired_amount"`
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
		price, err := strconv.ParseFloat(bid.LimitPrice, 64)
		if err != nil {
			return Book{}, fmt.Errorf("parse bid price: %w", err)
		}
		book.Bids = append(book.Bids, BookLevel{
			Price: price,
			Size:  rawToFloat(bid.DesiredAmount),
		})
	}
	for _, ask := range resp.Asks {
		price, err := strconv.ParseFloat(ask.LimitPrice, 64)
		if err != nil {
			return Book{}, fmt.Errorf("parse ask price: %w", err)
		}
		book.Asks = append(book.Asks, BookLevel{
			Price: price,
			Size:  rawToFloat(ask.DesiredAmount),
		})
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
	positions, err := c.readAccountBalances(ctx)
	if err != nil {
		return nil, err
	}

	type exposure struct {
		base  float64
		quote float64
	}
	exposures := make(map[string]exposure)
	rows, err := c.pg.Query(ctx, `
select side, desired_amount, limit_price, asset_address, sub_id
from active_orders
where owner_address = $1 and status = 'active'
`, c.cfg.OwnerAddress)
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
		key := strings.ToLower(assetAddress) + "|" + subID
		value := exposures[key]
		if Side(side) == SideBuy {
			value.quote += size * px
		} else {
			value.base += size
		}
		exposures[key] = value
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]Balance, 0, 2)
	for _, spec := range c.markets {
		if spec.AssetAddress == "" || spec.QuoteAddress == "" {
			continue
		}
		key := strings.ToLower(spec.AssetAddress) + "|" + spec.SubID
		baseTotal := positions[key]
		baseReserved := exposures[key].base
		baseAvailable := maxFloat(0, baseTotal-baseReserved)
		out = append(out, Balance{
			Asset:     spec.BaseAsset,
			Total:     baseTotal,
			Reserved:  baseReserved,
			Available: baseAvailable,
		})

		quoteKey := strings.ToLower(spec.QuoteAddress) + "|0"
		quoteTotal := positions[quoteKey]
		quoteReserved := exposures[key].quote
		quoteAvailable := maxFloat(0, quoteTotal-quoteReserved)
		out = append(out, Balance{
			Asset:     spec.QuoteAsset,
			Total:     quoteTotal,
			Reserved:  quoteReserved,
			Available: quoteAvailable,
		})
		break
	}
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
		out = append(out, Order{
			ID:         orderID,
			Market:     market,
			Side:       Side(side),
			Price:      price,
			Size:       rawToFloat(remainingRaw),
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

	rawAmount := floatToRaw(req.Size)
	actionData, err := encodeTradeData(spec.AssetAddress, spec.SubID, req.Price, rawAmount, c.cfg.RecipientID, req.Side == SideBuy, c.cfg.WorstFee)
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

	payload := map[string]any{
		"order_id":       req.OrderID,
		"owner_address":  c.cfg.OwnerAddress,
		"signer_address": c.cfg.SignerAddress,
		"subaccount_id":  c.cfg.SubaccountID,
		"recipient_id":   c.cfg.RecipientID,
		"nonce":          req.Nonce,
		"side":           req.Side,
		"asset_address":  spec.AssetAddress,
		"sub_id":         spec.SubID,
		"desired_amount": rawAmount,
		"filled_amount":  "0",
		"limit_price":    normalizePrice(req.Price),
		"worst_fee":      c.cfg.WorstFee,
		"expiry":         expiry,
		"action_json":    actionJSON,
		"signature":      signature,
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
	tm, _ := time.Parse(time.RFC3339Nano, resp.Order.CreatedAt)
	return Order{
		ID:         resp.Order.OrderID,
		Market:     req.Market,
		Side:       resp.Order.Side,
		Price:      price,
		Size:       rawToFloat(resp.Order.DesiredAmount),
		RawSize:    resp.Order.DesiredAmount,
		Nonce:      resp.Order.Nonce,
		Owner:      resp.Order.OwnerAddress,
		CreatedAt:  tm,
		Managed:    true,
		Subaccount: resp.Order.SubaccountID,
	}, nil
}

func (c *HTTPClient) CancelOrder(ctx context.Context, orderID string) error {
	orders, err := c.lookupOrderByID(ctx, orderID)
	if err != nil {
		return err
	}
	body := map[string]string{
		"owner_address": c.cfg.OwnerAddress,
		"nonce":         orders.Nonce,
	}
	return c.post(ctx, "/v1/orders/cancel", body, nil)
}

func (c *HTTPClient) CancelAllOrders(ctx context.Context, market string) error {
	orders, err := c.ListOpenOrders(ctx, market)
	if err != nil {
		return err
	}
	for _, order := range orders {
		if err := c.CancelOrder(ctx, order.ID); err != nil && !strings.Contains(err.Error(), "active order not found") {
			return err
		}
	}
	return nil
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
	return Order{
		ID:         id,
		Side:       Side(side),
		Price:      price,
		Size:       rawToFloat(desiredAmount),
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
	}
	if err := c.get(ctx, "/v1/markets", nil, &resp); err != nil {
		return err
	}
	for _, item := range resp {
		tickSize, _ := strconv.ParseFloat(item.TickSize, 64)
		spec := MarketSpec{
			Symbol:       item.Market,
			BaseAsset:    item.BaseAssetSymbol,
			QuoteAsset:   item.QuoteAssetSymbol,
			AssetAddress: strings.ToLower(item.AssetAddress),
			SubID:        defaultString(item.SubID, "0"),
			TickSize:     tickSize,
			QuoteAddress: strings.ToLower(c.quoteAsset.Hex()),
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

func (c *HTTPClient) readAccountBalances(ctx context.Context) (map[string]float64, error) {
	subaccountID, ok := new(big.Int).SetString(c.cfg.SubaccountID, 10)
	if !ok {
		return nil, fmt.Errorf("invalid subaccount id %q", c.cfg.SubaccountID)
	}
	callABI, err := abi.JSON(strings.NewReader(`[
{"name":"getAccountBalances","type":"function","stateMutability":"view","inputs":[{"name":"accountId","type":"uint256"}],"outputs":[{"name":"assetBalances","type":"tuple[]","components":[{"name":"asset","type":"address"},{"name":"subId","type":"uint256"},{"name":"balance","type":"int256"}]}]},
{"name":"quoteAsset","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"address"}]}
]`))
	if err != nil {
		return nil, err
	}
	input, err := callABI.Pack("getAccountBalances", subaccountID)
	if err != nil {
		return nil, err
	}
	output, err := c.rpc.CallContract(ctx, ethereumCallMsg(c.subAccounts, input), nil)
	if err != nil {
		return nil, fmt.Errorf("getAccountBalances rpc: %w", err)
	}
	values, err := callABI.Unpack("getAccountBalances", output)
	if err != nil {
		return nil, fmt.Errorf("decode account balances: %w", err)
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
	if ok {
		for _, item := range items {
			positions[strings.ToLower(item.Asset.Hex())+"|"+item.SubId.String()] = rawBigToFloat(item.Balance)
		}
		return positions, nil
	}

	generic, ok := values[0].([]balanceRow)
	if !ok {
		return nil, fmt.Errorf("unexpected balance payload %T", values[0])
	}
	for _, item := range generic {
		positions[strings.ToLower(item.Asset.Hex())+"|"+item.SubId.String()] = rawBigToFloat(item.Balance)
	}
	return positions, nil
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
		SubID         *big.Int
		LimitPrice    *big.Int
		DesiredAmount *big.Int
		WorstFee      *big.Int
		RecipientID   *big.Int
		IsBid         bool
	}{
		Asset:         common.HexToAddress(assetAddress),
		SubID:         subIDInt,
		LimitPrice:    priceInt,
		DesiredAmount: amountInt,
		WorstFee:      worstFeeInt,
		RecipientID:   recipientInt,
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
	rat := new(big.Rat).SetFloat64(value)
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

func subtractRaw(left, right string) (string, error) {
	leftInt, ok := new(big.Int).SetString(strings.TrimSpace(left), 10)
	if !ok {
		return "", fmt.Errorf("invalid raw decimal %q", left)
	}
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
