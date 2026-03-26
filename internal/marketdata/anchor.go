package marketdata

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/numofx/market-maker/internal/config"
)

var cngnOracleABI = mustParseABI(`[
{"name":"decimals","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"uint8"}]},
{"name":"latestRoundData","type":"function","stateMutability":"view","inputs":[],"outputs":[
  {"name":"roundId","type":"uint80"},
  {"name":"answer","type":"int256"},
  {"name":"startedAt","type":"uint256"},
  {"name":"updatedAt","type":"uint256"},
{"name":"answeredInRound","type":"uint80"}]}
]`)

func mustParseABI(raw string) abi.ABI {
	parsed, err := abi.JSON(strings.NewReader(raw))
	if err != nil {
		panic(err)
	}
	return parsed
}

type AnchorSource interface {
	Name() string
	GetAnchorPrice(ctx context.Context, market string) (float64, error)
}

type NoopAnchorSource struct{}

func (NoopAnchorSource) Name() string { return "none" }
func (NoopAnchorSource) GetAnchorPrice(context.Context, string) (float64, error) {
	return 0, nil
}

type FixedAnchorSource struct {
	price float64
}

func (s FixedAnchorSource) Name() string { return "fixed" }
func (s FixedAnchorSource) GetAnchorPrice(context.Context, string) (float64, error) {
	return s.price, nil
}

type HTTPAnchorSource struct {
	baseURL string
	client  *http.Client
}

func (s HTTPAnchorSource) Name() string { return "http" }
func (s HTTPAnchorSource) GetAnchorPrice(ctx context.Context, market string) (float64, error) {
	u, err := url.Parse(s.baseURL)
	if err != nil {
		return 0, err
	}
	query := u.Query()
	query.Set("market", market)
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("anchor endpoint returned %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	var body struct {
		Price float64 `json:"price"`
	}
	if err := json.Unmarshal(raw, &body); err == nil && body.Price > 0 {
		return body.Price, nil
	}
	if price, err := strconv.ParseFloat(strings.TrimSpace(string(raw)), 64); err == nil {
		return price, nil
	}
	return 0, fmt.Errorf("anchor response missing parseable price")
}

func NewAnchorSource(cfg config.Config) AnchorSource {
	switch cfg.AnchorSourceType {
	case "fixed":
		return FixedAnchorSource{price: cfg.AnchorFixedPrice}
	case "http":
		return HTTPAnchorSource{
			baseURL: cfg.AnchorURL,
			client:  &http.Client{Timeout: 5 * time.Second},
		}
	default:
		return NoopAnchorSource{}
	}
}

type ExternalAnchorQuote struct {
	Price            float64
	Present          bool
	FetchedAt        time.Time
	RefreshAttempted bool
	RefreshFailed    bool
}

type USDCCNGNSpotExternalAnchor interface {
	Fetch(ctx context.Context) ExternalAnchorQuote
}

type NoopUSDCCNGNSpotExternalAnchor struct{}

func (NoopUSDCCNGNSpotExternalAnchor) Fetch(context.Context) ExternalAnchorQuote {
	return ExternalAnchorQuote{}
}

type ZeroExUSDCCNGNSpotExternalAnchor struct {
	cfg      config.USDCCNGNSpotExternalAnchorConfig
	client   *http.Client
	mu       sync.Mutex
	last     ExternalAnchorQuote
	decimals *uint8
}

func NewUSDCCNGNSpotExternalAnchor(cfg config.Config) USDCCNGNSpotExternalAnchor {
	if !cfg.USDCCNGNSpotExternalAnchor.Enabled || cfg.MarketSymbol != "USDCcNGN-SPOT" {
		return NoopUSDCCNGNSpotExternalAnchor{}
	}
	return &ZeroExUSDCCNGNSpotExternalAnchor{
		cfg:    cfg.USDCCNGNSpotExternalAnchor,
		client: &http.Client{Timeout: cfg.USDCCNGNSpotExternalAnchor.Timeout},
	}
}

func (s *ZeroExUSDCCNGNSpotExternalAnchor) Fetch(ctx context.Context) ExternalAnchorQuote {
	quote, err := s.fetchFresh(ctx)
	if err != nil {
		s.logFailure(err)
		cached := s.cachedIfFresh()
		cached.RefreshAttempted = true
		cached.RefreshFailed = true
		return cached
	}
	if quote.Price <= 0 {
		s.logFailure(fmt.Errorf("parsed non-positive price"))
		cached := s.cachedIfFresh()
		cached.RefreshAttempted = true
		cached.RefreshFailed = true
		return cached
	}
	if !quote.FetchedAt.IsZero() && time.Since(quote.FetchedAt) > s.cfg.MaxAge {
		s.logFailure(fmt.Errorf("anchor quote older than max age"))
		cached := s.cachedIfFresh()
		cached.RefreshAttempted = true
		cached.RefreshFailed = true
		return cached
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last.Present && s.cfg.MaxDeviationBPS > 0 {
		deviation := absBPS(quote.Price, s.last.Price)
		if deviation > s.cfg.MaxDeviationBPS {
			slog.Warn("external anchor rejected", "provider", s.cfg.Provider, "market", "USDCcNGN-SPOT", "reason", "deviation_guard", "candidate_price", quote.Price, "last_price", s.last.Price, "deviation_bps", deviation)
			if time.Since(s.last.FetchedAt) <= s.cfg.MaxAge {
				cached := s.last
				cached.RefreshAttempted = true
				cached.RefreshFailed = true
				return cached
			}
			return ExternalAnchorQuote{RefreshAttempted: true, RefreshFailed: true}
		}
	}
	s.last = quote
	return quote
}

func (s *ZeroExUSDCCNGNSpotExternalAnchor) fetchFresh(ctx context.Context) (ExternalAnchorQuote, error) {
	if s.cfg.Provider == "cngn-price-oracle" {
		return s.fetchCNGNOracleOnChain(ctx)
	}
	u, err := url.Parse(s.cfg.BaseURL)
	if err != nil {
		return ExternalAnchorQuote{}, err
	}
	query := u.Query()
	query.Set("chainId", strconv.FormatInt(s.cfg.ChainID, 10))
	query.Set("sellToken", s.cfg.SellToken)
	query.Set("buyToken", s.cfg.BuyToken)
	query.Set("sellAmount", s.cfg.Amount)
	u.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return ExternalAnchorQuote{}, err
	}
	req.Header.Set("0x-version", "v2")
	if s.cfg.APIKey != "" {
		req.Header.Set("0x-api-key", s.cfg.APIKey)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return ExternalAnchorQuote{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if err != nil {
		return ExternalAnchorQuote{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return ExternalAnchorQuote{}, &ExternalAnchorFetchError{
			StatusCode:  resp.StatusCode,
			BodyPreview: truncateBody(string(raw)),
			Err:         fmt.Errorf("endpoint returned %d", resp.StatusCode),
		}
	}
	price, err := parseZeroExPrice(raw)
	if err != nil {
		return ExternalAnchorQuote{}, &ExternalAnchorFetchError{
			StatusCode:  resp.StatusCode,
			BodyPreview: truncateBody(string(raw)),
			Err:         err,
		}
	}
	return ExternalAnchorQuote{
		Price:            price,
		Present:          true,
		FetchedAt:        time.Now().UTC(),
		RefreshAttempted: true,
	}, nil
}

func (s *ZeroExUSDCCNGNSpotExternalAnchor) fetchCNGNOracleOnChain(ctx context.Context) (ExternalAnchorQuote, error) {
	oracleAddress := common.HexToAddress("0xdfbb5Cbc88E382de007bfe6CE99C388176ED80aD")
	decimals, err := s.cngnOracleDecimals(ctx, oracleAddress)
	if err != nil {
		return ExternalAnchorQuote{}, err
	}
	input, err := cngnOracleABI.Pack("latestRoundData")
	if err != nil {
		return ExternalAnchorQuote{}, err
	}
	raw, err := s.rpcCall(ctx, oracleAddress, input)
	if err != nil {
		return ExternalAnchorQuote{}, err
	}
	values, err := cngnOracleABI.Unpack("latestRoundData", raw)
	if err != nil {
		return ExternalAnchorQuote{}, err
	}
	if len(values) != 5 {
		return ExternalAnchorQuote{}, fmt.Errorf("unexpected latestRoundData output count %d", len(values))
	}
	answer, ok := values[1].(*big.Int)
	if !ok || answer == nil {
		return ExternalAnchorQuote{}, fmt.Errorf("unexpected answer type %T", values[1])
	}
	if answer.Sign() <= 0 {
		return ExternalAnchorQuote{}, fmt.Errorf("oracle answer must be positive")
	}
	updatedAt, ok := values[3].(*big.Int)
	if !ok || updatedAt == nil {
		return ExternalAnchorQuote{}, fmt.Errorf("unexpected updatedAt type %T", values[3])
	}
	scale := 1.0
	for i := uint8(0); i < decimals; i++ {
		scale *= 10
	}
	usdPerNGN := float64(answer.Int64()) / scale
	if usdPerNGN <= 0 {
		return ExternalAnchorQuote{}, fmt.Errorf("oracle usdPerNGN must be positive")
	}
	price := 1 / usdPerNGN
	fetchedAt := time.Unix(updatedAt.Int64(), 0).UTC()
	return ExternalAnchorQuote{
		Price:            price,
		Present:          true,
		FetchedAt:        fetchedAt,
		RefreshAttempted: true,
	}, nil
}

func (s *ZeroExUSDCCNGNSpotExternalAnchor) cngnOracleDecimals(ctx context.Context, oracleAddress common.Address) (uint8, error) {
	s.mu.Lock()
	if s.decimals != nil {
		value := *s.decimals
		s.mu.Unlock()
		return value, nil
	}
	s.mu.Unlock()

	input, err := cngnOracleABI.Pack("decimals")
	if err != nil {
		return 0, err
	}
	raw, err := s.rpcCall(ctx, oracleAddress, input)
	if err != nil {
		return 0, err
	}
	values, err := cngnOracleABI.Unpack("decimals", raw)
	if err != nil {
		return 0, err
	}
	value, ok := values[0].(uint8)
	if !ok {
		return 0, fmt.Errorf("unexpected decimals type %T", values[0])
	}
	s.mu.Lock()
	s.decimals = &value
	s.mu.Unlock()
	return value, nil
}

func (s *ZeroExUSDCCNGNSpotExternalAnchor) rpcCall(ctx context.Context, to common.Address, data []byte) ([]byte, error) {
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
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.RPCURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rpc returned %d", resp.StatusCode)
	}
	var out struct {
		Result string `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out.Error != nil {
		return nil, fmt.Errorf("rpc error: %s", out.Error.Message)
	}
	return hexutil.Decode(out.Result)
}

func (s *ZeroExUSDCCNGNSpotExternalAnchor) cachedIfFresh() ExternalAnchorQuote {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.last.Present {
		return ExternalAnchorQuote{}
	}
	if time.Since(s.last.FetchedAt) > s.cfg.MaxAge {
		return ExternalAnchorQuote{}
	}
	return s.last
}

func (s *ZeroExUSDCCNGNSpotExternalAnchor) logFailure(err error) {
	var fetchErr *ExternalAnchorFetchError
	if errors.As(err, &fetchErr) {
		slog.Warn("external anchor refresh failed", "provider", s.cfg.Provider, "market", "USDCcNGN-SPOT", "status_code", fetchErr.StatusCode, "body", fetchErr.BodyPreview, "error", fetchErr.Err)
		return
	}
	slog.Warn("external anchor refresh failed", "provider", s.cfg.Provider, "market", "USDCcNGN-SPOT", "error", err)
}

type ExternalAnchorFetchError struct {
	StatusCode  int
	BodyPreview string
	Err         error
}

func (e *ExternalAnchorFetchError) Error() string {
	if e == nil || e.Err == nil {
		return "external anchor fetch failed"
	}
	return e.Err.Error()
}

func (e *ExternalAnchorFetchError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func parseZeroExPrice(raw []byte) (float64, error) {
	var body struct {
		Price string `json:"price"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return 0, err
	}
	if body.Price == "" {
		return 0, fmt.Errorf("response missing price")
	}
	price, err := strconv.ParseFloat(body.Price, 64)
	if err != nil {
		return 0, fmt.Errorf("parse price: %w", err)
	}
	if price <= 0 {
		return 0, fmt.Errorf("price must be positive")
	}
	return price, nil
}

func parseCNGNOraclePrice(raw []byte) (float64, time.Time, error) {
	var body struct {
		USDToNGN  any    `json:"usdToNgn"`
		UpdatedAt string `json:"updatedAt"`
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return 0, time.Time{}, err
	}
	price, err := parseAnyFloat(body.USDToNGN)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("response missing usdToNgn: %w", err)
	}
	var fetchedAt time.Time
	for _, candidate := range []string{body.UpdatedAt, body.Timestamp} {
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		if ts, err := time.Parse(time.RFC3339Nano, candidate); err == nil {
			fetchedAt = ts.UTC()
			break
		}
	}
	return price, fetchedAt, nil
}

func parseAnyFloat(value any) (float64, error) {
	switch v := value.(type) {
	case float64:
		if v <= 0 {
			return 0, fmt.Errorf("value must be positive")
		}
		return v, nil
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, err
		}
		if parsed <= 0 {
			return 0, fmt.Errorf("value must be positive")
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("unsupported type %T", value)
	}
}

func truncateBody(raw string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) <= 256 {
		return raw
	}
	return raw[:256]
}

func absBPS(a, b float64) float64 {
	if a <= 0 || b <= 0 {
		return 0
	}
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return diff / b * 10000
}
