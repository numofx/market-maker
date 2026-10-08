package exchange

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	gethmath "github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/numofx/market-maker/internal/marketnames"
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
	// OrderID identifies the resting order behind the level, so a bot can tell its own quotes apart
	// from other participants'.
	OrderID string `json:"order_id"`
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
	// Reusable is the part of Reserved that this bot will free again this cycle: capacity held by
	// its own replaceable orders ON THIS MARKET. The quoting budget is Available + Reusable.
	//
	// Computed here, with the same arithmetic that produced Reserved, because deriving it a second
	// time elsewhere is what made the ladder size drift: the strategy recomputed the reservation
	// from UI values while this used engine values, so the add-back never cancelled the
	// subtraction and the residual fed the next cycle's size.
	//
	// Excluded from Reusable, and therefore genuinely unavailable:
	//   - protected orders (validation/smoke/manual), which are never cancelled by design
	//   - orders on OTHER markets in the same subaccount, which this ladder cannot free --
	//     the exposure query filters only by owner and subaccount, not by market
	Reusable float64 `json:"reusable"`
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
	// PostOnly and WorstFee are read back so startup can tell whether a resting order was signed
	// under the CURRENT configuration. An order placed by a previous image carries that image's
	// guarantees, and nothing else notices: a stable ladder is never replaced, so it can rest
	// indefinitely under terms the operator believes were superseded.
	PostOnly bool
	WorstFee string
	// Expiry is the signed action's expiry in unix seconds; 0 means unknown.
	//
	// A cancel is off-chain only -- TradeModule has no nonce invalidation -- so an order the venue
	// has "cancelled" stays executable on-chain until this moment. The bot therefore signs a short
	// rolling expiry and re-signs before it lapses, which it can only do if it reads the expiry back.
	Expiry int64 `json:"expiry"`
}

// MarketKind is what a market is, from /v1/markets: it decides what a size is and what backs a
// quote.
type MarketKind string

const (
	// MarketKindSpot is the cNGN spot market (cNGN-USDC, once USDCcNGN-SPOT): priced in USDC per
	// cNGN, sized in whole cNGN, a buy is a buy of cNGN; each side of a quote is backed by the token
	// it delivers.
	MarketKindSpot MarketKind = "spot"
	// MarketKindPerp is the cNGN perp (cNGN-PERP, once USDCcNGN-PERP): priced and sized like spot
	// (the engine holds the position in cNGN, long positive), but both sides are backed by cash
	// margin.
	MarketKindPerp MarketKind = "perp"
	// MarketKindFuture is a dated, cash-margined future sized in 0.001 contract lots and priced in
	// its own engine units; nothing about the cNGN orientation applies to it.
	MarketKindFuture MarketKind = "future"
)

// marketKind classifies a /v1/markets row. The order entry spec is the venue's own statement of
// the market, under either presentation (see Orientation); the symbol and contract type are kept
// as a fallback for a markets-service that omits it.
func marketKind(symbol, contractType, orderEntrySpec string) MarketKind {
	switch {
	case orderEntrySpec == SpecCNGNUSDCSpot || orderEntrySpec == SpecUSDCCNGNSpot || marketnames.IsSpot(symbol) || contractType == "spot":
		return MarketKindSpot
	case orderEntrySpec == SpecCNGNUSDCPerp || orderEntrySpec == SpecUSDCCNGNPerp || marketnames.IsPerp(symbol) || contractType == "perpetual":
		return MarketKindPerp
	default:
		return MarketKindFuture
	}
}

// PerpState is the perp's live chain state as /v1/markets serves it (the `perp` object), refreshed
// with the rest of the market schedule. Prices are the engine's: USDC per cNGN, whichever
// presentation the venue served them under (see perpPriceFromVenue).
type PerpState struct {
	TradeModule   string
	QuoteAsset    string
	MarginManager string
	MarkPrice     float64
	IndexPrice    float64
	// TradingEnabled is false until the enable vault action: the matcher skips the market, so quotes
	// would only rest.
	TradingEnabled bool
	// PositionCapNGN sums |position| over BOTH sides; OpenInterestNGN is one side.
	PositionCapNGN  float64
	OpenInterestNGN float64
	MaxLeverage     float64
	FetchedAt       time.Time
}

// SideRoomNGN is how much more one side of the market can open before the OI cap, in cNGN. A fill
// that opens both counterparties uses a unit of room on each side.
func (p PerpState) SideRoomNGN() float64 {
	room := p.PositionCapNGN/2 - p.OpenInterestNGN
	if room <= 0 {
		return 0
	}
	return room
}

// SideRoomUSD is SideRoomNGN valued in USDC at the index.
func (p PerpState) SideRoomUSD() float64 {
	if p.IndexPrice <= 0 {
		return 0
	}
	return p.SideRoomNGN() * p.IndexPrice
}

type MarketSpec struct {
	Kind MarketKind
	// Orientation is how the venue PRESENTS this market (see orientation.go). Every number on the
	// spec, and everywhere downstream, is already in engine terms; this is kept for logs and for
	// tools that read presented rows.
	Orientation Orientation
	// Perp is set for a perp market with a readable `perp` object; nil otherwise.
	Perp *PerpState
	// Symbol is the market's canonical identifier: the `market` /v1/markets lists it under. It is
	// what every order id, log line and comparison downstream uses, whatever the operator wrote in
	// MM_MARKET_SYMBOL.
	Symbol string
	// Aliases are the other identifiers the venue accepts for this market: its `aliases` from
	// /v1/markets, or, for a listing without them, the bot's own fallback table (marketnames). An
	// order id tagged with any of them is this bot's.
	Aliases []string
	// BaseAsset and QuoteAsset are the engine's: cNGN and USDC on the cNGN markets, whatever order
	// the venue listed them in.
	BaseAsset      string
	QuoteAsset     string
	AssetAddress   string
	QuoteAddress   string
	SubID          string
	TickSize       float64
	SizeStep       float64
	MinSize        float64
	OrderEntrySpec string
	// TakerFeeBps is the venue's taker fee for this market, in basis points of the quote
	// notional, exactly as /v1/markets reports it. It is never configured here: the matcher
	// charges what markets-service's instrument registry says, so a copy in this repo would be a
	// second source of truth that goes stale the first time the venue's changes.
	//
	// It exists so the bot can sign a worstFee that covers the charge. Zero means the market
	// reported no schedule, and the bot falls back to MM_WORST_FEE.
	TakerFeeBps int
	// ExpiryTimestamp is the market's expiry (unix seconds) from /v1/markets;
	// zero for spot / perpetual markets.
	ExpiryTimestamp int64
}

// Names is every identifier this market answers to: the canonical symbol first, then its aliases.
func (s MarketSpec) Names() []string {
	return append([]string{s.Symbol}, s.Aliases...)
}

// HasName reports whether name identifies this market, canonically or by alias. Exact match only,
// like the venue.
func (s MarketSpec) HasName(name string) bool {
	for _, known := range s.Names() {
		if name == known {
			return true
		}
	}
	return false
}

// IsManagedOrderID reports whether an order id was minted by this bot for this market: the
// "mm:<market>:" tag, under the market's canonical name or any of its aliases, so a ladder placed
// before the venue renamed the market is still recognised as this bot's after it.
func (s MarketSpec) IsManagedOrderID(orderID string) bool {
	for _, name := range s.Names() {
		if strings.HasPrefix(orderID, managedOrderPrefix(name)) {
			return true
		}
	}
	return false
}

// CNGNDenominated reports whether this is one of the cNGN markets: priced in USDC per cNGN and
// sized in whole cNGN, with the operator's USDC-denominated sizes and limits converted at the
// quote price. True for spot and the perp; a future keeps its own contract units.
func (s MarketSpec) CNGNDenominated() bool {
	kind := s.kind()
	return kind == MarketKindSpot || kind == MarketKindPerp
}

func (s MarketSpec) IsSpot() bool { return s.kind() == MarketKindSpot }

func (s MarketSpec) IsPerp() bool { return s.kind() == MarketKindPerp }

// kind is Kind when loadMarkets set it, else derived the same way from what the spec carries, so a
// spec built by hand (tests, tools) classifies exactly as a loaded one.
func (s MarketSpec) kind() MarketKind {
	if s.Kind != "" {
		return s.Kind
	}
	return marketKind(s.Symbol, "", s.OrderEntrySpec)
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

type Client interface {
	GetBook(ctx context.Context, market string) (Book, error)
	GetTrades(ctx context.Context, market string) ([]Trade, error)
	GetBalances(ctx context.Context) ([]Balance, error)
	ListOpenOrders(ctx context.Context, market string) ([]Order, error)
	PlaceLimitOrder(ctx context.Context, req PlaceOrderRequest) (Order, error)
	CancelOrder(ctx context.Context, orderID string, reason string) error
	CancelAllOrders(ctx context.Context, market string, reason string) error
	GetMarket(ctx context.Context, market string) (MarketSpec, error)
	RequiredWorstFee(spec MarketSpec, uiPrice float64) (string, error)
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
	// MarketRefreshSeconds bounds how stale the cached /v1/markets schedule may get before the
	// next GetMarket refetches it. Unset falls back to defaultRefreshEvery; there is no way to
	// switch the refresh off, because "never refresh" is the defect this exists to fix.
	MarketRefreshSeconds int64
	OrderExpirySeconds   int64
	ServiceName          string
	ProtectedPrefixes    []string
	// Signer, when set, is the only key: OwnerPrivateKey/SignerPrivateKey are ignored and the
	// configured addresses must equal Signer.Address(). See resolveSigner.
	Signer Signer
}

type PlaceOrderRequest struct {
	Market  string
	Side    Side
	Price   float64
	Size    float64
	OrderID string
	Nonce   string
	// PostOnly asks the venue to refuse the order rather than let it take. A market maker wants
	// this on every quote: taking is what it is trying not to do, and since the fee follows
	// whichever order arrived later, a quote that crosses pays the taker fee.
	PostOnly bool
}

type HTTPClient struct {
	cfg         ClientConfig
	httpClient  *http.Client
	pg          *pgxpool.Pool
	rpc         *ethclient.Client
	signer      Signer
	matching    common.Address
	tradeModule common.Address
	subAccounts common.Address
	quoteAsset  common.Address

	// markets is the fee schedule and instrument metadata as /v1/markets last reported it.
	//
	// It used to be written once in the constructor and never again, which made the signed
	// worstFee bound permanently whatever the venue was publishing at boot. A bot that started
	// before a schedule existed kept signing worstFee 0 forever, and since chooseTakerMaker makes
	// the later order the taker, a requoting bot IS the taker most of the time -- so every cross
	// reverted TM_FeeTooHigh until the process was restarted by hand.
	//
	// Guarded because refreshes now happen on the quoting path while other calls read it.
	marketsMu sync.RWMutex
	// markets is keyed by canonical symbol; marketNames maps every identifier the venue accepts
	// (canonical and alias) to that key. Both are replaced wholesale by loadMarkets.
	markets      map[string]MarketSpec
	marketNames  map[string]string
	marketsAt    time.Time
	marketsStale bool
	refreshEvery time.Duration
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
	if cfg.RecipientID == "" {
		cfg.RecipientID = cfg.SubaccountID
	}
	if cfg.WorstFee == "" {
		cfg.WorstFee = "0"
	}
	if cfg.OrderExpirySeconds <= 0 {
		// Matches config.defaultExpirySeconds: a cancel is off-chain only, so the signed expiry is
		// the real bound on how long a quote can be taken on-chain.
		cfg.OrderExpirySeconds = 60
	}
	if cfg.MatchingRepoPath == "" {
		cfg.MatchingRepoPath = "../execution-contracts"
	}
	if cfg.RiskCoreRepoPath == "" {
		cfg.RiskCoreRepoPath = "../risk-core"
	}

	signer, cfg, err := resolveSigner(cfg)
	if err != nil {
		return nil, err
	}

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
		cfg:          cfg,
		httpClient:   &http.Client{Timeout: 15 * time.Second},
		pg:           pg,
		rpc:          rpc,
		signer:       signer,
		matching:     common.HexToAddress(cfg.MatchingAddress),
		tradeModule:  common.HexToAddress(cfg.TradeModuleAddress),
		subAccounts:  common.HexToAddress(cfg.SubAccountsAddress),
		markets:      make(map[string]MarketSpec),
		refreshEvery: refreshInterval(cfg.MarketRefreshSeconds),
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

// defaultRefreshEvery is how long a fetched schedule is trusted. A fee change reaches the signed
// bound within a cycle or two, at the cost of one request a minute rather than one per order.
const defaultRefreshEvery = 60 * time.Second

// refreshInterval turns the configured seconds into a duration. A zero or negative value is an
// unset field rather than a request to disable refreshing -- disabling it is exactly the bug --
// so it falls back to the default.
func refreshInterval(seconds int64) time.Duration {
	if seconds <= 0 {
		return defaultRefreshEvery
	}
	return time.Duration(seconds) * time.Second
}

func (c *HTTPClient) Close() {
	if c.pg != nil {
		c.pg.Close()
	}
	if c.rpc != nil {
		c.rpc.Close()
	}
}

// GetMarket returns the market's metadata, refreshing it from /v1/markets when the cached copy
// has aged past refreshEvery.
//
// The refresh is what keeps the signed fee bound honest: PlaceLimitOrder derives worstFee from
// spec.TakerFeeBps, so a cache that never expires signs a bound for a schedule the venue stopped
// publishing. See the note on the markets field.
//
// A FAILED refresh keeps the last good schedule and returns it. It must never fall back to a zero
// TakerFeeBps, because signedWorstFee reads zero as "this market publishes no schedule" and
// substitutes MM_WORST_FEE -- reintroducing the exact zero bound this is here to prevent. An RPC
// blip would otherwise turn into a venue-wide revert loop. The staleness is recorded instead, and
// surfaces on the next quote cycle.
func (c *HTTPClient) GetMarket(ctx context.Context, market string) (MarketSpec, error) {
	c.marketsMu.RLock()
	spec, ok := c.markets[c.canonicalNameLocked(market)]
	fresh := time.Since(c.marketsAt) < c.refreshEvery
	c.marketsMu.RUnlock()

	if ok && fresh {
		return spec, nil
	}

	if err := c.loadMarkets(ctx); err != nil {
		if ok {
			c.marketsMu.Lock()
			c.marketsStale = true
			c.marketsMu.Unlock()
			slog.Warn(
				"market_refresh_failed",
				"market", market,
				"error", err,
				"effect", "keeping the last known schedule; the signed fee bound may be stale",
				"taker_fee_bps", spec.TakerFeeBps,
			)
			return spec, nil
		}
		// Nothing was ever loaded, so there is no last-good to fall back to. Refusing is right:
		// quoting here would sign against a schedule we have never seen.
		return MarketSpec{}, err
	}

	c.marketsMu.RLock()
	defer c.marketsMu.RUnlock()
	spec, ok = c.markets[c.canonicalNameLocked(market)]
	if !ok {
		return MarketSpec{}, fmt.Errorf("unknown market %q: the venue lists %v", market, c.listedNamesLocked())
	}
	return spec, nil
}

// canonicalNameLocked maps any identifier the venue accepts for a market to the key markets is
// held under. An unknown name maps to itself, so the lookup that follows fails as before. Callers
// hold marketsMu.
func (c *HTTPClient) canonicalNameLocked(name string) string {
	if canonical, ok := c.marketNames[name]; ok {
		return canonical
	}
	return name
}

// listedNamesLocked is every identifier the venue accepts, canonical and alias, sorted, for the
// error that refuses an unknown one. Callers hold marketsMu.
func (c *HTTPClient) listedNamesLocked() []string {
	names := make([]string, 0, len(c.marketNames))
	for name := range c.marketNames {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// lookupMarket resolves a market by any identifier the venue accepts, from the schedule in hand.
func (c *HTTPClient) lookupMarket(name string) (MarketSpec, bool) {
	c.marketsMu.RLock()
	defer c.marketsMu.RUnlock()
	spec, ok := c.markets[c.canonicalNameLocked(name)]
	return spec, ok
}

// SpotMarket is the canonical name of the venue's cNGN spot market as listed, if it is listed: the
// spot reference market the strategy's spot-only pricing keys on. It is resolved from the listing's
// own statement of what each market is, never from a name.
func (c *HTTPClient) SpotMarket() (string, bool) {
	c.marketsMu.RLock()
	defer c.marketsMu.RUnlock()
	for symbol, spec := range c.markets {
		if spec.IsSpot() {
			return symbol, true
		}
	}
	return "", false
}

// MarketsStale reports whether the last refresh attempt failed, so the schedule in hand is older
// than it should be. Read by the bot for its metrics; it is deliberately not an error, because a
// stale-but-known schedule is safe to quote against and a hard stop would be worse.
func (c *HTTPClient) MarketsStale() bool {
	c.marketsMu.RLock()
	defer c.marketsMu.RUnlock()
	return c.marketsStale
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
	if spec.IsSpot() {
		return "CNGN_SPOT_ASSET_ADDRESS"
	}
	if spec.IsPerp() {
		return "CNGN_PERP_ASSET_ADDRESS"
	}
	return "MARKET_ASSET_ADDRESS"
}

func (c *HTTPClient) GetBook(ctx context.Context, market string) (Book, error) {
	spec, err := c.GetMarket(ctx, market)
	if err != nil {
		return Book{}, err
	}
	// The book is read from its engine fields -- limit_price in USDC per cNGN, desired_amount in
	// whole cNGN, bids are engine bids -- which markets-service serves identically under both
	// presentations. spot_contract.ui_intent is the venue's display projection and is ignored.
	type bookRow struct {
		OrderID       string `json:"order_id"`
		LimitPrice    string `json:"limit_price"`
		DesiredAmount string `json:"desired_amount"`
	}
	var resp struct {
		Bids []bookRow `json:"bids"`
		Asks []bookRow `json:"asks"`
	}
	if err := c.get(ctx, "/v1/book", url.Values{"symbol": []string{market}}, &resp); err != nil {
		return Book{}, err
	}
	book := Book{
		Bids: make([]BookLevel, 0, len(resp.Bids)),
		Asks: make([]BookLevel, 0, len(resp.Asks)),
	}
	for _, bid := range resp.Bids {
		level, err := parseBookLevel(spec, bid.LimitPrice, bid.DesiredAmount)
		if err != nil {
			return Book{}, err
		}
		level.OrderID = bid.OrderID
		book.Bids = append(book.Bids, level)
	}
	for _, ask := range resp.Asks {
		level, err := parseBookLevel(spec, ask.LimitPrice, ask.DesiredAmount)
		if err != nil {
			return Book{}, err
		}
		level.OrderID = ask.OrderID
		book.Asks = append(book.Asks, level)
	}
	return book, nil
}

func (c *HTTPClient) GetTrades(ctx context.Context, market string) ([]Trade, error) {
	spec, err := c.GetMarket(ctx, market)
	if err != nil {
		return nil, err
	}
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
		// Trades are stored engine-side: price in USDC per cNGN, size in cNGN, the aggressor's
		// engine side. That is the bot's own orientation, so nothing is converted.
		out = append(out, Trade{
			ID:        item.TradeID,
			Price:     price,
			Size:      rawOrderSizeToFloat(spec, item.Size),
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
	if spec, specErr := c.marketForBalances(); specErr == nil && spec.IsPerp() {
		return PerpBalances(spec, positions)
	}

	exposures := make(map[string]float64)
	reusable := make(map[string]float64)
	rows, err := c.pg.Query(ctx, `
select order_id, side, desired_amount, limit_price, asset_address, sub_id
from active_orders
where owner_address = $1 and subaccount_id = $2 and status = 'active'
`, c.cfg.OwnerAddress, c.cfg.SubaccountID)
	if err != nil {
		return nil, fmt.Errorf("query exposures: %w", err)
	}
	defer rows.Close()
	marketSpec, specErr := c.marketForBalances()
	for rows.Next() {
		var orderID string
		var side string
		var rawSize string
		var price string
		var assetAddress string
		var subID string
		if err := rows.Scan(&orderID, &side, &rawSize, &price, &assetAddress, &subID); err != nil {
			return nil, fmt.Errorf("scan exposure: %w", err)
		}
		size := orderAmountToFloat(c.marketSpecForAsset(assetAddress, subID), rawSize)
		px, err := strconv.ParseFloat(price, 64)
		if err != nil {
			return nil, fmt.Errorf("parse exposure price: %w", err)
		}
		assetKey, reserved := c.reservedExposureKey(side, size, px, assetAddress, subID)
		exposures[assetKey] += reserved
		// Reusable only when this ladder will actually free it: this market, not protected.
		sameMarket := specErr == nil &&
			strings.EqualFold(assetAddress, marketSpec.AssetAddress) && subID == marketSpec.SubID
		if sameMarket && !isProtectedOrderID(orderID, c.cfg.ProtectedPrefixes) {
			reusable[assetKey] += reserved
		}
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
	baseTotal := rawPositionToMarketSize(spec, positions[baseKey])
	baseReserved := exposures[baseKey]
	baseAvailable := maxFloat(0, baseTotal-baseReserved)
	slog.Info("subaccount balance mapped", "market", spec.Symbol, "subaccount_id", c.cfg.SubaccountID, "asset", spec.BaseAsset, "asset_address", strings.Split(baseKey, "|")[0], "sub_id", strings.Split(baseKey, "|")[1], "total", baseTotal, "reserved", baseReserved, "available", baseAvailable)
	out = append(out, Balance{
		Asset:     spec.BaseAsset,
		Total:     baseTotal,
		Reserved:  baseReserved,
		Available: baseAvailable,
		Reusable:  minFloat(baseReserved, reusable[baseKey]),
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
		Reusable:  minFloat(quoteReserved, reusable[quoteKey]),
	})
	return dedupeBalances(out), nil
}

// PerpBalances maps a perp account onto the two balances the strategy reads:
//
//   - base (cNGN): the position exactly as the engine holds it, a SIGNED cNGN amount, long cNGN
//     positive. The strategy and risk layers value it in USDC at the reference price when they
//     compare it with MM_MAX_LONG/SHORT_INVENTORY.
//   - quote (USDC): the perp's cash, the margin collateral.
//
// Nothing is reserved against resting orders. A perp order reserves margin, not notional, and the
// strategy budgets from cash Total (see strategy.perpCapacity), so a notional reservation here
// would double-count exactly as it did for futures.
func PerpBalances(spec MarketSpec, positions map[string]float64) ([]Balance, error) {
	if spec.Perp == nil || spec.Perp.IndexPrice <= 0 {
		return nil, fmt.Errorf("perp %s has no index in /v1/markets: cannot value the position", spec.Symbol)
	}
	baseKey, quoteKey := balanceKeys(spec)
	positionNGN := positions[baseKey]
	cash := positions[quoteKey]
	slog.Info("perp account mapped",
		"market", spec.Symbol,
		"position_cngn", positionNGN,
		"position_usd", positionNGN*spec.Perp.IndexPrice,
		"cash_usd", cash,
		"index_usdc_per_cngn", spec.Perp.IndexPrice,
	)
	return []Balance{
		{Asset: spec.BaseAsset, Total: positionNGN, Available: positionNGN},
		{Asset: spec.QuoteAsset, Total: cash, Available: maxFloat(0, cash)},
	}, nil
}

func (c *HTTPClient) ListOpenOrders(ctx context.Context, market string) ([]Order, error) {
	spec, err := c.GetMarket(ctx, market)
	if err != nil {
		return nil, err
	}

	rows, err := c.pg.Query(ctx, `
select order_id, side, limit_price, desired_amount, filled_amount, nonce, owner_address, created_at, subaccount_id,
       post_only, worst_fee, coalesce(expiry, 0)
from active_orders
where owner_address = $1 and asset_address = $2 and sub_id = $3 and subaccount_id = $4 and status = 'active'
order by created_at asc
`, c.cfg.OwnerAddress, strings.ToLower(spec.AssetAddress), spec.SubID, c.cfg.SubaccountID)
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
			postOnly      bool
			worstFee      string
			expiry        int64
		)
		if err := rows.Scan(&orderID, &side, &limitPrice, &desiredAmount, &filledAmount, &nonce, &owner, &createdAt, &subaccountID, &postOnly, &worstFee, &expiry); err != nil {
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
			Size:       orderAmountToFloat(spec, remainingRaw),
			RawSize:    remainingRaw,
			Nonce:      nonce,
			Owner:      owner,
			CreatedAt:  createdAt,
			Managed:    spec.IsManagedOrderID(orderID),
			Subaccount: subaccountID,
			PostOnly:   postOnly,
			WorstFee:   worstFee,
			Expiry:     expiry,
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

	// The request is already in engine terms on every market: side as the engine matches it,
	// price in the engine's units, size in the engine's units. Nothing here translates; the only
	// work is quantising to what the venue accepts.
	engineSide := req.Side
	enginePrice := req.Price
	engineAmount := req.Size
	payloadSide := req.Side
	payloadDesiredAmount := floatToRaw(req.Size)
	signedDesiredAmount := marketSizeToRawOrder(spec, req.Size)
	payloadLimitPrice := normalizePrice(req.Price)
	payload := map[string]any{}
	if spec.CNGNDenominated() {
		if !(req.Price > 0) || !(req.Size > 0) {
			return Order{}, fmt.Errorf("order price and size must be positive, got %v x %v", req.Price, req.Size)
		}
		// markets-service enforces a whole-cNGN atomic step ("1") on the cNGN markets'
		// desired_amount. The order is submitted engine-native (side/limit_price/desired_amount,
		// no order_entry_spec or ui_intent), so it is valid under either presentation the venue
		// serves: floor the amount to a whole cNGN and send it directly.
		engineAmount = math.Floor(engineAmount)
		if engineAmount < 1 {
			return Order{}, fmt.Errorf("order amount %v is below 1 cNGN", req.Size)
		}
		payloadDesiredAmount = strconv.FormatFloat(engineAmount, 'f', -1, 64)
		signedDesiredAmount = floatToRaw(engineAmount)
		// The signed action carries the price as an 18-decimal fixed-point raw value
		// (floatToRaw). Render the SAME quantized value as the body limit_price so it
		// aligns to the 1e-18 tick and matches the signature exactly — the float's
		// shortest decimal form can exceed 18 decimals and get rejected.
		payloadLimitPrice = rawPriceToDecimalString(floatToRaw(enginePrice))
	} else {
		// Cash-margined / deliverable FUTURE: the request BODY carries the human-decimal
		// contract quantity while the SIGNED action carries the on-chain wei amount. Round
		// the requested size down to the MinSize step so markets-service normalization aligns.
		bodyDecimal, signedWei, roundedSize, ferr := futureOrderAmounts(spec, req.Size)
		if ferr != nil {
			return Order{}, ferr
		}
		engineAmount = roundedSize
		payloadDesiredAmount = bodyDecimal
		signedDesiredAmount = signedWei
	}
	// The bound is derived from this order's own engine price and the venue's published
	// schedule, not from a configured constant. A quote signed with a bound below the schedule
	// rests, crosses, and then reverts TM_FeeTooHigh on chain -- and because chooseTakerMaker
	// makes the LATER order the taker, a bot that requotes is the taker most of the time, so the
	// zero bound that was correct while the venue charged nothing became a permanent revert loop
	// the moment it started charging.
	worstFee, err := c.signedWorstFee(spec, enginePrice)
	if err != nil {
		return Order{}, err
	}
	actionData, err := encodeTradeData(spec.AssetAddress, spec.SubID, enginePrice, signedDesiredAmount, c.cfg.RecipientID, engineSide == SideBuy, worstFee)
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
	signature, err := c.signAction(ctx, actionJSON)
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
	payload["worst_fee"] = worstFee
	payload["expiry"] = expiry
	payload["action_json"] = actionJSON
	payload["signature"] = signature
	payload["side"] = payloadSide
	payload["desired_amount"] = payloadDesiredAmount
	payload["limit_price"] = payloadLimitPrice
	if req.PostOnly {
		payload["post_only"] = true
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
		Size:       orderAmountToFloat(spec, resp.Order.DesiredAmount),
		RawSize:    resp.Order.DesiredAmount,
		Nonce:      resp.Order.Nonce,
		Owner:      resp.Order.OwnerAddress,
		CreatedAt:  tm,
		Managed:    true,
		Subaccount: resp.Order.SubaccountID,
		PostOnly:   req.PostOnly,
		WorstFee:   worstFee,
		Expiry:     expiry,
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
	expiry := strconv.FormatInt(time.Now().UTC().Add(cancelSignatureLifetime).Unix(), 10)
	signature, err := c.signCancel(ctx, c.cfg.OwnerAddress, c.cfg.SignerAddress, orders.Nonce, expiry)
	if err != nil {
		return fmt.Errorf("sign cancel for %s: %w", orderID, err)
	}
	body := map[string]string{
		"owner_address":  c.cfg.OwnerAddress,
		"signer_address": c.cfg.SignerAddress,
		"nonce":          orders.Nonce,
		"expiry":         expiry,
		"signature":      signature,
		"reason":         machineCancelReason(c.cfg.ServiceName, reason),
		"service":        normalizeCancelToken(c.cfg.ServiceName),
	}
	if err := c.post(ctx, "/v1/orders/cancel", body, nil); err != nil {
		if strings.Contains(err.Error(), "active order not found") {
			return fmt.Errorf("%w: %w", ErrOrderNotFound, err)
		}
		return err
	}
	return nil
}

// ErrOrderNotFound is a cancel for an order that is no longer resting: already filled, already
// cancelled, or mid-settlement (status "matching", which active_orders no longer serves as active).
//
// Not an error for a cancel-everything path. The order is off the book either way, and treating it
// as a failure would make a kill report failure precisely when a fill raced it.
var ErrOrderNotFound = errors.New("active order not found")

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
select order_id, side, limit_price, desired_amount, nonce, owner_address, created_at, subaccount_id, asset_address, sub_id,
       coalesce(expiry, 0)
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
		assetAddress  string
		subID         string
		expiry        int64
	)
	if err := row.Scan(&id, &side, &limitPrice, &desiredAmount, &nonce, &owner, &createdAt, &subaccountID, &assetAddress, &subID, &expiry); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Order{}, fmt.Errorf("lookup order %s: %w: %w", orderID, ErrOrderNotFound, err)
		}
		return Order{}, fmt.Errorf("lookup order %s: %w", orderID, err)
	}
	price, _ := strconv.ParseFloat(limitPrice, 64)
	size := orderAmountToFloat(c.marketSpecForAsset(assetAddress, subID), desiredAmount)
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
		Expiry:     expiry,
	}, nil
}

func (c *HTTPClient) loadMarkets(ctx context.Context) error {
	var resp []struct {
		Market string `json:"market"`
		// Aliases are the other identifiers the venue accepts for the market (its pre-rename
		// names). A listing that omits the field entirely is served from the fallback table.
		Aliases          []string `json:"aliases"`
		BaseAssetSymbol  string   `json:"base_asset_symbol"`
		QuoteAssetSymbol string   `json:"quote_asset_symbol"`
		AssetAddress     string   `json:"asset_address"`
		SubID            string   `json:"sub_id"`
		TickSize         string   `json:"tick_size"`
		OrderEntrySpec   string   `json:"order_entry_spec"`
		TakerFeeBps      int      `json:"taker_fee_bps"`
		ExpiryTimestamp  int64    `json:"expiry_timestamp"`
		ContractType     string   `json:"contract_type"`
		Perp             *struct {
			MarkPrice      string `json:"mark_price"`
			IndexPrice     string `json:"index_price"`
			MarkPriceUI    string `json:"mark_price_ui"`
			IndexPriceUI   string `json:"index_price_ui"`
			OpenInterest   string `json:"open_interest"`
			MaxLeverage    string `json:"max_leverage"`
			TradeModule    string `json:"trade_module_address"`
			QuoteAsset     string `json:"quote_asset_address"`
			MarginManager  string `json:"margin_manager_address"`
			TradingEnabled bool   `json:"trading_enabled"`
			PositionCap    string `json:"position_cap"`
		} `json:"perp"`
	}
	if err := c.get(ctx, "/v1/markets", nil, &resp); err != nil {
		return err
	}

	// Built into a fresh map and swapped in at the end, so a failure part-way through leaves the
	// previous schedule intact rather than a half-updated one.
	next := make(map[string]MarketSpec, len(resp))
	names := make(map[string]string, 2*len(resp))
	for _, item := range resp {
		tickSize, _ := strconv.ParseFloat(item.TickSize, 64)
		kind := marketKind(item.Market, item.ContractType, item.OrderEntrySpec)
		aliases := item.Aliases
		if aliases == nil {
			// The venue said nothing about aliases (an old markets-service, or a new one that
			// does not publish them yet): the bot's own table covers both spellings.
			aliases = marketnames.FallbackAliases(item.Market)
		}
		spec := MarketSpec{
			Symbol:          item.Market,
			Aliases:         aliasesOf(item.Market, aliases),
			BaseAsset:       item.BaseAssetSymbol,
			QuoteAsset:      item.QuoteAssetSymbol,
			AssetAddress:    strings.ToLower(item.AssetAddress),
			SubID:           defaultString(item.SubID, "0"),
			TickSize:        tickSize,
			QuoteAddress:    strings.ToLower(c.quoteAsset.Hex()),
			OrderEntrySpec:  item.OrderEntrySpec,
			TakerFeeBps:     item.TakerFeeBps,
			ExpiryTimestamp: item.ExpiryTimestamp,
			Kind:            kind,
			Orientation:     OrientationEngine,
		}
		if spec.CNGNDenominated() {
			// The one place the venue's presentation is read. Everything on the spec is engine
			// terms from here on: cNGN is the base, USDC the quote, prices USDC per cNGN.
			spec.Orientation = orientationOf(item.Market, item.OrderEntrySpec)
			spec.BaseAsset, spec.QuoteAsset = spec.Orientation.EngineSymbols(item.BaseAssetSymbol, item.QuoteAssetSymbol)
		}
		if spec.IsPerp() && item.Perp != nil {
			spec.Perp = &PerpState{
				TradeModule:     strings.ToLower(item.Perp.TradeModule),
				QuoteAsset:      strings.ToLower(item.Perp.QuoteAsset),
				MarginManager:   strings.ToLower(item.Perp.MarginManager),
				MarkPrice:       perpPriceFromVenue(item.Perp.MarkPrice, item.Perp.MarkPriceUI, spec.Orientation),
				IndexPrice:      perpPriceFromVenue(item.Perp.IndexPrice, item.Perp.IndexPriceUI, spec.Orientation),
				TradingEnabled:  item.Perp.TradingEnabled,
				PositionCapNGN:  parseFloatOrZero(item.Perp.PositionCap),
				OpenInterestNGN: parseFloatOrZero(item.Perp.OpenInterest),
				MaxLeverage:     parseFloatOrZero(item.Perp.MaxLeverage),
				FetchedAt:       time.Now().UTC(),
			}
		}
		switch {
		case spec.CNGNDenominated():
			// Spot and the perp are sized in whole cNGN: the venue's atomic amount step is 1.
			spec.SizeStep = 1
			spec.MinSize = 1
		case item.Market == "USDCcNGN-APR30-2026", item.Market == "USDCcNGN-SEP16-2026", item.Market == "USDCcNGN-NOV30-2026", item.Market == "USDCcNGN-MAY31-2027":
			// cNGN deliverable FX futures: markets-service enforces a 0.001 atomic
			// amount step (registry MinSize) for these symbols; the order body must
			// align to it (see futureOrderAmounts).
			spec.SizeStep = 0.001
			spec.MinSize = 0.001
		default:
			// Any other market is a future settled in 0.001 contract steps. Spot and the
			// perp are the 0.000001 markets and are cased explicitly above, so defaulting
			// to 0.001 keeps a newly listed future expiry aligned without a code change.
			spec.SizeStep = 0.001
			spec.MinSize = 0.001
		}
		next[item.Market] = spec
		for _, name := range spec.Names() {
			names[name] = item.Market
		}
	}
	if len(next) == 0 {
		// Do NOT clear the cache here. An empty response is far more likely to be a bad deploy or
		// a half-started service than a venue that genuinely delisted everything, and the caller
		// treats a returned error as "keep the last good schedule".
		return fmt.Errorf("no markets returned by exchange")
	}

	c.marketsMu.Lock()
	previous := c.markets
	c.markets = next
	c.marketNames = names
	c.marketsAt = time.Now()
	c.marketsStale = false
	c.marketsMu.Unlock()

	// A fee change is the reason this refresh exists, so say so once when it happens rather than
	// leaving it to be inferred from reverts.
	for symbol, spec := range next {
		// The previous schedule may hold this market under another of its names: the venue
		// renaming a market mid-run is not a fee change.
		was, existed := previous[symbol]
		for _, alias := range spec.Aliases {
			if existed {
				break
			}
			was, existed = previous[alias]
		}
		if existed && was.TakerFeeBps != spec.TakerFeeBps {
			slog.Info(
				"taker_fee_schedule_changed",
				"market", symbol,
				"from_bps", was.TakerFeeBps,
				"to_bps", spec.TakerFeeBps,
				"effect", "new orders sign a bound derived from the new schedule",
			)
		}
	}
	return nil
}

// marketsSnapshot returns the current schedule map. loadMarkets replaces the map wholesale and
// never mutates one that has been published, so the returned reference stays safe to range over
// after the lock is dropped -- a later refresh swaps in a different map rather than editing this
// one.
func (c *HTTPClient) marketsSnapshot() map[string]MarketSpec {
	c.marketsMu.RLock()
	defer c.marketsMu.RUnlock()
	return c.markets
}

func (c *HTTPClient) marketForBalances() (MarketSpec, error) {
	if c.cfg.MarketSymbol != "" {
		// By any identifier the venue accepts: the operator may still configure the market under
		// its pre-rename name.
		spec, ok := c.lookupMarket(c.cfg.MarketSymbol)
		if !ok {
			return MarketSpec{}, fmt.Errorf("configured market %s not loaded", c.cfg.MarketSymbol)
		}
		return spec, nil
	}
	for _, spec := range c.marketsSnapshot() {
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

// CheckPerpWiring refuses a perp run whose configuration does not match the venue's perp stack. The
// bot signs for one module (MM_TRADE_MODULE_ADDRESS) and reads its cash as that module's
// quoteAsset(), so both must be the perp's; and its subaccount must sit under the perp SRM, or the
// orders would be margined by a manager that knows nothing about the perp. Spot and futures pass.
func (c *HTTPClient) CheckPerpWiring(ctx context.Context, spec MarketSpec) error {
	if !spec.IsPerp() {
		return nil
	}
	if spec.Perp == nil {
		return fmt.Errorf("%s: /v1/markets serves no perp state", spec.Symbol)
	}
	if !strings.EqualFold(c.tradeModule.Hex(), spec.Perp.TradeModule) {
		return fmt.Errorf("MM_TRADE_MODULE_ADDRESS %s is not the perp's module %s", c.tradeModule.Hex(), spec.Perp.TradeModule)
	}
	if !strings.EqualFold(c.quoteAsset.Hex(), spec.Perp.QuoteAsset) {
		return fmt.Errorf("the module's quoteAsset %s is not the perp's cash %s", c.quoteAsset.Hex(), spec.Perp.QuoteAsset)
	}
	subaccountID, ok := new(big.Int).SetString(c.cfg.SubaccountID, 10)
	if !ok {
		return fmt.Errorf("invalid subaccount id %q", c.cfg.SubaccountID)
	}
	managerABI, err := abi.JSON(strings.NewReader(`[{"name":"manager","type":"function","stateMutability":"view","inputs":[{"name":"accountId","type":"uint256"}],"outputs":[{"name":"","type":"address"}]}]`))
	if err != nil {
		return err
	}
	input, err := managerABI.Pack("manager", subaccountID)
	if err != nil {
		return err
	}
	output, err := c.rpc.CallContract(ctx, ethereumCallMsg(c.subAccounts, input), nil)
	if err != nil {
		return fmt.Errorf("SubAccounts.manager rpc: %w", err)
	}
	values, err := managerABI.Unpack("manager", output)
	if err != nil || len(values) != 1 {
		return fmt.Errorf("decode SubAccounts.manager: %v", err)
	}
	manager, _ := values[0].(common.Address)
	if !strings.EqualFold(manager.Hex(), spec.Perp.MarginManager) {
		return fmt.Errorf("MM_SUBACCOUNT_ID %s is under manager %s, not the perp SRM %s", c.cfg.SubaccountID, manager.Hex(), spec.Perp.MarginManager)
	}
	return nil
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

func (c *HTTPClient) signAction(ctx context.Context, action map[string]string) (string, error) {
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
	sig, err := c.signer.SignHash(ctx, hash)
	if err != nil {
		return "", fmt.Errorf("sign typed data: %w", err)
	}
	sig[64] += 27
	return hexutil.Encode(sig), nil
}

// cancelSignatureLifetime bounds how long a signed cancel can be replayed. It only has to cover the
// request round trip and any clock skew against markets-service, so it is kept short.
const cancelSignatureLifetime = 2 * time.Minute

// signCancel signs the Cancel(owner,signer,nonce,expiry) authorization markets-service verifies
// before removing a resting order. It mirrors signAction over the same Matching domain; the server
// requires signer == owner for cancels (there is no off-chain session-key registry), which holds
// here because the bot signs its own orders with its owner key.
func (c *HTTPClient) signCancel(ctx context.Context, owner, signer, nonce, expiry string) (string, error) {
	td := apitypes.TypedData{
		Types: apitypes.Types{
			"EIP712Domain": {
				{Name: "name", Type: "string"},
				{Name: "version", Type: "string"},
				{Name: "chainId", Type: "uint256"},
				{Name: "verifyingContract", Type: "address"},
			},
			"Cancel": {
				{Name: "owner", Type: "address"},
				{Name: "signer", Type: "address"},
				{Name: "nonce", Type: "uint256"},
				{Name: "expiry", Type: "uint256"},
			},
		},
		PrimaryType: "Cancel",
		Domain: apitypes.TypedDataDomain{
			Name:              "Matching",
			Version:           "1.0",
			ChainId:           (*gethmath.HexOrDecimal256)(big.NewInt(c.cfg.ChainID)),
			VerifyingContract: c.matching.Hex(),
		},
		Message: apitypes.TypedDataMessage{
			"owner":  owner,
			"signer": signer,
			"nonce":  nonce,
			"expiry": expiry,
		},
	}
	hash, _, err := apitypes.TypedDataAndHash(td)
	if err != nil {
		return "", fmt.Errorf("hash cancel typed data: %w", err)
	}
	sig, err := c.signer.SignHash(ctx, hash)
	if err != nil {
		return "", fmt.Errorf("sign cancel typed data: %w", err)
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

// ErrPostOnlyWouldCross is the venue refusing a post-only order because it would have taken.
//
// It is a routine outcome, not a failure: the book moved between quoting and submitting, which on
// a fast market is expected several times an hour. Callers skip the level and requote rather than
// abandoning the cycle.
var ErrPostOnlyWouldCross = errors.New("post_only order would cross the resting book")

func (c *HTTPClient) do(req *http.Request, out any) error {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		// 422 with this message is the post-only guard, which the caller treats as an expected
		// outcome rather than an error. Matching on the status AND the text so an unrelated 422
		// does not get quietly swallowed as "just a reprice".
		if resp.StatusCode == http.StatusUnprocessableEntity && strings.Contains(string(body), "post_only order would cross") {
			return ErrPostOnlyWouldCross
		}
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

// RequiredWorstFee is the bound this process would sign for an order already resting at the given
// price -- used by startup reconciliation and the steady-state cycle to decide whether a resting
// order's signed bound still covers the current fee schedule.
//
// Order.Price is the engine price on every market (USDC per cNGN on the cNGN markets), which is
// exactly what signedWorstFee bounds: the fee per cNGN filled is the rate times the USDC each cNGN
// is worth. A caller that passed a cNGN-per-USDC number here would get a bound ~1.8 million times
// too large and adopt every stale order; there is no such number anywhere in the bot any more.
func (c *HTTPClient) RequiredWorstFee(spec MarketSpec, price float64) (string, error) {
	return c.signedWorstFee(spec, price)
}

// worstFeeHeadroomBps is how far above the venue's published schedule the bot signs its fee
// ceiling. A ceiling signed at exactly the schedule bricks every resting quote the instant the
// schedule moves up by one basis point, and requoting is not instant -- the orders already on the
// book were signed under the old number. Five basis points is the same headroom the trading app
// signs (30 over a 25 bps schedule), so the two agree about how much slack a fee change has.
const worstFeeHeadroomBps = 5

// signedWorstFee is the per-unit fee ceiling to sign into an order on this market.
//
// TradeModule bounds fee/amountFilled, not the total: _fillLimitOrder reverts TM_FeeTooHigh when
// the charge per filled unit exceeds worstFee. amountFilled is denominated in the base asset, so
// the bound is a QUOTE amount PER BASE UNIT -- which makes it price-dependent, and a single
// configured constant wrong at every price but one.
//
// For one base unit worth enginePrice of quote, a feeBps charge on the notional is
// feeBps/10_000 * enginePrice per unit. The result is rounded UP: the matcher truncates when it
// computes the fee, so a bound rounded down can sit one wei under a charge that is otherwise
// exactly at the ceiling.
//
// A market that reports no schedule falls back to the configured MM_WORST_FEE unchanged. That is
// the futures path, where the notional is the contract's rather than the price's, and nothing
// here should silently start signing a different bound for it.
func (c *HTTPClient) signedWorstFee(spec MarketSpec, enginePrice float64) (string, error) {
	if spec.TakerFeeBps <= 0 {
		return c.cfg.WorstFee, nil
	}
	if !(enginePrice > 0) {
		return "", fmt.Errorf("cannot bound fee at engine price %v", enginePrice)
	}

	bound := new(big.Rat).Mul(
		ratFromFloat(enginePrice),
		big.NewRat(int64(spec.TakerFeeBps+worstFeeHeadroomBps), 10_000),
	)
	bound.Mul(bound, new(big.Rat).SetInt(decimalScale))

	units := new(big.Int).Quo(bound.Num(), bound.Denom())
	if new(big.Int).Mul(units, bound.Denom()).Cmp(bound.Num()) != 0 {
		units.Add(units, big.NewInt(1))
	}
	if units.Sign() == 0 {
		// The bound truncated to nothing, so any fee at all breaches it. One wei is the smallest
		// ceiling that is not a guaranteed revert.
		units.SetInt64(1)
	}
	return units.String(), nil
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

// ratFromFloat converts a float64 to an exact big.Rat via its shortest decimal
// representation, avoiding binary float drift (e.g. 0.1 -> 0.10000000000000000555).
func ratFromFloat(value float64) *big.Rat {
	normalized := normalizeDecimalString(strconv.FormatFloat(value, 'f', -1, 64))
	if rat, ok := new(big.Rat).SetString(normalized); ok {
		return rat
	}
	return new(big.Rat).SetFloat64(value)
}

// futureOrderAmounts derives the two amount representations a cash-margined /
// deliverable future order requires:
//
//   - bodyDecimal: the human-decimal contract quantity carried in the request BODY
//     `desired_amount`. markets-service parses this as a decimal and divides it by the
//     instrument MinSize (the atomic step) to obtain an integer atomic contract count.
//   - signedWei: the on-chain wei amount carried in the SIGNED action (encodeTradeData),
//     which is 1e18 (assetDecimals) per whole contract.
//
// The requested size is first rounded DOWN to the MinSize step so the body always
// normalizes to a whole atomic count and the signed amount stays an exact integer
// multiple of that count (see markets-service inferSharedScale / validateActionDataInvariants).
func futureOrderAmounts(spec MarketSpec, size float64) (bodyDecimal string, signedWei string, roundedSize float64, err error) {
	step := ratFromFloat(spec.MinSize)
	if step.Sign() <= 0 {
		return "", "", 0, fmt.Errorf("invalid min size %v for market %s", spec.MinSize, spec.Symbol)
	}
	sizeRat := ratFromFloat(size)
	if sizeRat.Sign() < 0 {
		return "", "", 0, fmt.Errorf("invalid order size %v", size)
	}
	// atomic = floor(size / step) using exact integer arithmetic.
	atomic := new(big.Int).Quo(
		new(big.Int).Mul(sizeRat.Num(), step.Denom()),
		new(big.Int).Mul(sizeRat.Denom(), step.Num()),
	)
	if atomic.Sign() <= 0 {
		return "", "", 0, fmt.Errorf("order size %v rounds below min size %v for market %s", size, spec.MinSize, spec.Symbol)
	}
	// rounded contract quantity = atomic * step
	rounded := new(big.Rat).Mul(new(big.Rat).SetInt(atomic), step)
	bodyDecimal = normalizeDecimalString(rounded.FloatString(assetDecimals))
	// signed wei = rounded * 10^assetDecimals (exact, since rounded is a multiple of step)
	weiRat := new(big.Rat).Mul(rounded, new(big.Rat).SetInt(decimalScale))
	signedWei = new(big.Int).Quo(weiRat.Num(), weiRat.Denom()).String()
	roundedSize, _ = rounded.Float64()
	return bodyDecimal, signedWei, roundedSize, nil
}

func rawOrderSizeToFloat(spec MarketSpec, raw string) float64 {
	if spec.CNGNDenominated() {
		// cNGN-market amounts are stored by markets-service in atomic whole-cNGN units
		// (amount step "1"), not wei — the raw value IS the engine cNGN amount.
		if n, ok := new(big.Rat).SetString(strings.TrimSpace(raw)); ok {
			size, _ := n.Float64()
			return size
		}
	}
	size := rawToFloat(raw)
	if usesContractLots(spec) {
		return size * spec.SizeStep
	}
	return size
}

// orderAmountToFloat converts a resting ORDER's stored desired_amount into a decimal
// contract size. markets-service stores — and presents unchanged — a future's order
// amount as an ATOMIC COUNT (an integer number of MinSize steps), so the decimal size is
// count * MinSize (e.g. "14" -> 0.014 at MinSize 0.001). Spot orders keep the legacy
// wei-based scaling. NOTE: trade fills use a different (wei) scale and must NOT go through
// here — they stay on rawOrderSizeToFloat.
func orderAmountToFloat(spec MarketSpec, raw string) float64 {
	if usesContractLots(spec) {
		if n, ok := new(big.Rat).SetString(strings.TrimSpace(raw)); ok {
			size, _ := new(big.Rat).Mul(n, ratFromFloat(spec.MinSize)).Float64()
			return size
		}
	}
	return rawOrderSizeToFloat(spec, raw)
}

func marketSizeToRawOrder(spec MarketSpec, size float64) string {
	if usesContractLots(spec) {
		return floatToRaw(size / spec.SizeStep)
	}
	return floatToRaw(size)
}

func rawPositionToMarketSize(spec MarketSpec, position float64) float64 {
	if usesContractLots(spec) {
		return position * spec.SizeStep
	}
	return position
}

func usesContractLots(spec MarketSpec) bool {
	return spec.Symbol != "" && spec.kind() == MarketKindFuture && spec.SizeStep > 0
}

func (c *HTTPClient) marketSpecForAsset(assetAddress, subID string) MarketSpec {
	for _, spec := range c.marketsSnapshot() {
		if strings.EqualFold(spec.AssetAddress, assetAddress) && spec.SubID == subID {
			return spec
		}
	}
	return MarketSpec{}
}

func normalizePrice(price float64) string {
	return strconv.FormatFloat(price, 'f', -1, 64)
}

// rawPriceToDecimalString renders an 18-decimal fixed-point raw value (as produced
// by floatToRaw) back as a plain decimal string, trimming trailing zeros.
func rawPriceToDecimalString(raw string) string {
	rat, ok := new(big.Rat).SetString(strings.TrimSpace(raw))
	if !ok {
		return raw
	}
	rat.Quo(rat, new(big.Rat).SetInt(decimalScale))
	return normalizeDecimalString(rat.FloatString(assetDecimals))
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

func parseBookLevel(spec MarketSpec, rawPrice, rawAmount string) (BookLevel, error) {
	price, err := strconv.ParseFloat(rawPrice, 64)
	if err != nil {
		return BookLevel{}, fmt.Errorf("parse book price: %w", err)
	}
	return BookLevel{Price: price, Size: orderAmountToFloat(spec, rawAmount)}, nil
}

// balanceKeys is the (asset|subId) key of the base and quote balances on chain. On every market the
// base is the market's own asset (cNGN on spot, the perp asset on the perp, the contract on a
// future) and the quote is the trade module's quoteAsset (USDC, cash).
func balanceKeys(spec MarketSpec) (string, string) {
	return strings.ToLower(spec.AssetAddress) + "|" + spec.SubID, strings.ToLower(spec.QuoteAddress) + "|0"
}

// reservedExposureKey is what a resting engine order holds: a bid (a buy of the base) reserves
// size x price of the quote; an ask reserves size of the base.
func (c *HTTPClient) reservedExposureKey(side string, size float64, px float64, assetAddress string, subID string) (string, float64) {
	for _, spec := range c.marketsSnapshot() {
		if strings.ToLower(spec.AssetAddress) != strings.ToLower(assetAddress) || spec.SubID != subID {
			continue
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

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
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

// aliasesOf is the listed aliases less the canonical name itself and any repeats, in order.
func aliasesOf(canonical string, listed []string) []string {
	var out []string
	for _, name := range listed {
		name = strings.TrimSpace(name)
		if name == "" || name == canonical {
			continue
		}
		duplicate := false
		for _, seen := range out {
			if seen == name {
				duplicate = true
				break
			}
		}
		if !duplicate {
			out = append(out, name)
		}
	}
	return out
}

func ethereumCallMsg(to common.Address, data []byte) ethereum.CallMsg {
	return ethereum.CallMsg{To: &to, Data: data}
}

func parseFloatOrZero(raw string) float64 {
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0
	}
	return value
}
