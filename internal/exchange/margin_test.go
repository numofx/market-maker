package exchange

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The cNGN future as markets-service lists it (services/markets internal/api/future.go): the engine as
// margin_manager_address, the contract size in the future block.
func TestLoadMarketsReadsTheFuturesManagerAndContractSize(t *testing.T) {
	row := map[string]any{
		"market": "USDC-cNGN-31DEC26", "contract_type": "future", "settlement_type": "cash_settled_future",
		"base_asset_symbol": "USDC", "quote_asset_symbol": "cNGN",
		"asset_address": "0x00000000000000000000000000000000000000F0", "sub_id": "1798675200", "expiry_timestamp": 1798675200,
		"tick_size": "0.000000000000000001", "taker_fee_bps": 25,
		"margin_manager_address": "0x00000000000000000000000000000000000000E9",
		"fill_band_enforced":     true, "fill_band_low": "1260", "fill_band_high": "1540",
		"future": map[string]any{"contract_size_usdc": "1000", "band_status": "ok"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{row})
	}))
	defer srv.Close()
	client := &HTTPClient{cfg: ClientConfig{APIBaseURL: srv.URL, WorstFee: "0"}, httpClient: &http.Client{Timeout: 5 * time.Second},
		markets: make(map[string]MarketSpec), refreshEvery: time.Minute}
	spec, err := client.GetMarket(context.Background(), "USDC-cNGN-31DEC26")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Kind != MarketKindFuture || spec.SubID != "1798675200" {
		t.Fatalf("spec: %+v", spec)
	}
	if spec.MarginManager != "0x00000000000000000000000000000000000000e9" || spec.ContractSizeUSDC != 1000 {
		t.Fatalf("manager %q, contract size %v", spec.MarginManager, spec.ContractSizeUSDC)
	}
}

func TestFutureIMHeadroomDecodesTheEngine(t *testing.T) {
	wad := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	word := func(v *big.Int) string {
		if v.Sign() < 0 {
			v = new(big.Int).Add(v, new(big.Int).Lsh(big.NewInt(1), 256))
		}
		return fmt.Sprintf("%064x", v)
	}
	margin := new(big.Int).Mul(big.NewInt(2500), wad)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage   `json:"id"`
			Params []json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var call struct {
			To    string `json:"to"`
			Input string `json:"input"`
			Data  string `json:"data"`
		}
		_ = json.Unmarshal(req.Params[0], &call)
		data := call.Input + call.Data
		var result string
		switch {
		case strings.HasPrefix(data, "0x623bb445"): // getMargin(uint256,bool)
			if !strings.HasSuffix(data, "1") {
				t.Errorf("getMargin must ask for initial margin: %s", data)
			}
			result = word(margin)
		case strings.HasPrefix(data, "0xd21415a3"): // assetDetails(address): whitelisted, Future, market 1
			result = word(big.NewInt(1)) + word(big.NewInt(4)) + word(big.NewInt(1))
		case strings.HasPrefix(data, "0x84033e5e"): // futureMarginRequirements(uint256): 20% MM, 25% IM
			result = word(new(big.Int).Div(wad, big.NewInt(5))) + word(new(big.Int).Div(wad, big.NewInt(4)))
		default:
			t.Errorf("unexpected call %s", data)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": "0x" + result})
	}))
	defer srv.Close()
	rpc, err := ethclient.Dial(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &HTTPClient{cfg: ClientConfig{SubaccountID: "803"}, rpc: rpc}
	spec := MarketSpec{AssetAddress: "0x00000000000000000000000000000000000000f0", MarginManager: "0x00000000000000000000000000000000000000e9", ContractSizeUSDC: 1000}

	surplus, perContract, err := client.FutureIMHeadroom(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if surplus != 2500 || perContract != 250 {
		t.Fatalf("surplus %v per contract %v, expected 2500 and 250", surplus, perContract)
	}
	margin = new(big.Int).Neg(new(big.Int).Mul(big.NewInt(40), wad))
	if surplus, _, err = client.FutureIMHeadroom(context.Background(), spec); err != nil || surplus != -40 {
		t.Fatalf("a short account's surplus is negative: %v %v", surplus, err)
	}
	if _, _, err := client.FutureIMHeadroom(context.Background(), MarketSpec{}); err == nil {
		t.Fatal("a market with no manager has no headroom to read")
	}
}

// Against the venue's own schema (markets-service migrations): set MM_TEST_VENUE_DATABASE_URL to a
// database they have been applied to. A cancelled order reports its cancel_reason; a filled one "";
// another subaccount's order is not this bot's to read.
func TestCancelReasonsReadsTheVenuesActiveOrders(t *testing.T) {
	url := os.Getenv("MM_TEST_VENUE_DATABASE_URL")
	if url == "" {
		// The test that proves the subaccount filter must not pass CI by skipping.
		if os.Getenv("CI") != "" {
			t.Fatal("MM_TEST_VENUE_DATABASE_URL is not set under CI (.github/workflows/test.yml migrates a venue database for it)")
		}
		t.Skip("MM_TEST_VENUE_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pg, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	prefix := fmt.Sprintf("mm-reasons-%d", time.Now().UnixNano())
	t.Cleanup(func() { _, _ = pg.Exec(ctx, "delete from active_orders where order_id like $1", prefix+"%") })
	insert := func(id, subaccount, status, reason string) {
		if _, err := pg.Exec(ctx, `
insert into active_orders (order_id, owner_address, signer_address, subaccount_id, recipient_id, nonce, side, asset_address, sub_id,
  desired_amount, filled_amount, limit_price, limit_price_ticks, worst_fee, expiry, action_json, signature, status, cancel_reason)
values ($1, '0xowner', '0xowner', $2, $2, $5, 'sell', '0xf0', '1798675200', '1', '0', '1400', '1400', '0', 0, '{}', '0x01', $3, nullif($4, ''))`,
			prefix+id, subaccount, status, reason, time.Now().UnixNano()); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	insert("-margin", "803", "cancelled", CancelReasonInsufficientMargin)
	insert("-filled", "803", "filled", "")
	insert("-other", "804", "cancelled", CancelReasonInsufficientMargin)
	client := &HTTPClient{cfg: ClientConfig{SubaccountID: "803"}, pg: pg}
	reasons, err := client.CancelReasons(ctx, []string{prefix + "-margin", prefix + "-filled", prefix + "-other"})
	if err != nil {
		t.Fatal(err)
	}
	if reasons[prefix+"-margin"] != CancelReasonInsufficientMargin || reasons[prefix+"-filled"] != "" {
		t.Fatalf("reasons: %v", reasons)
	}
	if _, ok := reasons[prefix+"-other"]; ok {
		t.Fatalf("another subaccount's order: %v", reasons)
	}
}

// futureChainRPC answers the future's reads: an index of 1/1400 USDC per cNGN, a 10% band, a fixing
// window of a day before an expiry of 1798675200, last trade an hour before expiry, margin 2,500 USD.
func futureChainRPC(t *testing.T, calls map[string]int) *httptest.Server {
	wad := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	word := func(v *big.Int) string { return fmt.Sprintf("%064x", v) }
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage   `json:"id"`
			Params []json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var call struct{ To, Input, Data string }
		_ = json.Unmarshal(req.Params[0], &call)
		data := call.Input + call.Data
		calls[data[:10]]++
		var result string
		switch data[:10] {
		case "0x623bb445": // getMargin(uint256,bool)
			result = word(new(big.Int).Mul(big.NewInt(2500), wad))
		case "0xd21415a3": // assetDetails(address)
			result = word(big.NewInt(1)) + word(big.NewInt(4)) + word(big.NewInt(1))
		case "0x84033e5e": // futureMarginRequirements(uint256)
			result = word(new(big.Int).Div(wad, big.NewInt(5))) + word(new(big.Int).Div(wad, big.NewInt(4)))
		case "0x198cae76": // indexFeed()
			result = word(big.NewInt(0xfe))
		case "0xf4360442": // fixingWindow()
			result = word(big.NewInt(86400))
		case "0xc3c74b34": // futureContracts(uint256): listed, not settled, expiry, last trade, ...
			result = word(big.NewInt(1)) + word(big.NewInt(0)) + word(big.NewInt(1798675200)) + word(big.NewInt(1798675200-3600)) +
				word(big.NewInt(0)) + word(new(big.Int).Mul(big.NewInt(1400), wad)) + word(big.NewInt(0))
		case "0x2b37269c": // getSpot()
			result = word(new(big.Int).Div(wad, big.NewInt(1400))) + word(wad)
		case "0x968564c5": // markIndexBand(uint64)
			result = word(new(big.Int).Div(wad, big.NewInt(10)))
		default:
			t.Errorf("unexpected call %s", data[:10])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": "0x" + result})
	}))
}

func TestFutureStateReadsTheBandAndScheduleAsTheChainHasThem(t *testing.T) {
	calls := map[string]int{}
	srv := futureChainRPC(t, calls)
	defer srv.Close()
	rpc, err := ethclient.Dial(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &HTTPClient{cfg: ClientConfig{SubaccountID: "803", FutureCutoffBeforeFixSeconds: 3600}, rpc: rpc}
	spec := MarketSpec{Kind: MarketKindFuture, AssetAddress: "0x00000000000000000000000000000000000000f0", SubID: "1798675200",
		MarginManager: "0x00000000000000000000000000000000000000e9", ContractSizeUSDC: 1000}
	got, err := client.FutureState(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	// 1e36 / floor(1e18/1400) = 1400.000000000000000280
	if math.Abs(got.Index-1400) > 1e-9 || math.Abs(got.BandLow-1260) > 1e-9 || math.Abs(got.BandHigh-1540) > 1e-9 {
		t.Fatalf("index %v band %v-%v", got.Index, got.BandLow, got.BandHigh)
	}
	if got.IMSurplus != 2500 || got.PerContractIM != 250 {
		t.Fatalf("margin %v per contract %v", got.IMSurplus, got.PerContractIM)
	}
	if got.FixingStart.Unix() != 1798675200-86400 || got.LastTradeTime.Unix() != 1798675200-3600 {
		t.Fatalf("schedule: fixing %v last trade %v", got.FixingStart, got.LastTradeTime)
	}
	// the cutoff is an hour before the fixing window opens (it opens before last trade here)
	if want := time.Unix(1798675200-86400-3600, 0).UTC(); !got.QuotingCutoff(time.Hour).Equal(want) || !client.futureOrderExpiryCap().Equal(want) {
		t.Fatalf("cutoff %v / cap %v, want %v", got.QuotingCutoff(time.Hour), client.futureOrderExpiryCap(), want)
	}
	// the schedule is read once; the index and band every cycle
	if _, err := client.FutureState(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if calls["0xf4360442"] != 1 || calls["0x198cae76"] != 1 || calls["0x2b37269c"] != 2 || calls["0x968564c5"] != 2 {
		t.Fatalf("reads: %v", calls)
	}
}

func TestAnOrderOnTheEngineFutureNeverOutlivesTheCutoff(t *testing.T) {
	future := MarketSpec{Kind: MarketKindFuture, ContractSizeUSDC: 1000}
	now := time.Unix(1_800_000_000, 0)
	cutoff := now.Add(10 * time.Minute)
	if got, err := cappedOrderExpiry(future, now.Add(time.Hour).Unix(), cutoff, now); err != nil || got != cutoff.Unix() {
		t.Fatalf("capped at the cutoff: %v %v", got, err)
	}
	if got, err := cappedOrderExpiry(future, now.Add(time.Minute).Unix(), cutoff, now); err != nil || got != now.Add(time.Minute).Unix() {
		t.Fatalf("an earlier expiry is kept: %v %v", got, err)
	}
	if _, err := cappedOrderExpiry(future, now.Add(time.Hour).Unix(), now.Add(-time.Second), now); err == nil {
		t.Fatal("past the cutoff nothing is signed")
	}
	if _, err := cappedOrderExpiry(future, now.Add(time.Hour).Unix(), time.Time{}, now); err == nil {
		t.Fatal("an unread schedule signs nothing")
	}
	other := MarketSpec{Kind: MarketKindPerp}
	if got, _ := cappedOrderExpiry(other, 42, time.Time{}, now); got != 42 {
		t.Fatal("other markets are untouched")
	}
}

// The future's fee is on contracts x 1,000 USDC: at 25 bps plus the 5 bps headroom, 3e18 per contract,
// whatever the price in cNGN. The old path bounded it at 30 bps of the price (4.2e18 at 1,400).
func TestTheEngineFutureWorstFeeIsOnTheContractSize(t *testing.T) {
	c := &HTTPClient{cfg: ClientConfig{WorstFee: "0"}}
	spec := MarketSpec{Kind: MarketKindFuture, ContractSizeUSDC: 1000, TakerFeeBps: 25}
	for _, price := range []float64{1400, 1260, 1540} {
		got, err := c.signedWorstFee(spec, price)
		if err != nil || got != "3000000000000000000" {
			t.Fatalf("at %v: %s %v", price, got, err)
		}
	}
}

// The wiring: a real PlaceLimitOrder on the engine future, against a stub venue, signs an expiry capped at
// the cutoff and a 3e18 worstFee, and refuses outright once the cutoff has passed.
func TestPlacingOnTheEngineFutureSignsTheCapAndTheContractFee(t *testing.T) {
	var posted []map[string]any
	venue := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"market": "USDC-cNGN-31DEC26", "contract_type": "future", "base_asset_symbol": "USDC", "quote_asset_symbol": "cNGN",
				"asset_address": "0x00000000000000000000000000000000000000f0", "sub_id": "1798675200", "expiry_timestamp": 1798675200,
				"tick_size": "0.000000000000000001", "taker_fee_bps": 25,
				"margin_manager_address": "0x00000000000000000000000000000000000000e9",
				"future":                 map[string]any{"contract_size_usdc": "1000"},
			}})
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		posted = append(posted, body)
		_ = json.NewEncoder(w).Encode(map[string]any{"order": map[string]any{"order_id": body["order_id"], "side": body["side"],
			"limit_price": body["limit_price"], "desired_amount": body["desired_amount"], "nonce": body["nonce"], "created_at": time.Now().UTC()}})
	}))
	defer venue.Close()
	key, _ := crypto.HexToECDSA(strings.Repeat("11", 32))
	owner := crypto.PubkeyToAddress(key.PublicKey).Hex()
	client := &HTTPClient{
		cfg: ClientConfig{APIBaseURL: venue.URL, OwnerAddress: owner, SignerAddress: owner, SubaccountID: "803", RecipientID: "803",
			ChainID: 8453, OrderExpirySeconds: 86400, WorstFee: "0", FutureCutoffBeforeFixSeconds: 3600},
		httpClient: &http.Client{Timeout: 5 * time.Second}, markets: make(map[string]MarketSpec), refreshEvery: time.Minute,
		signer: NewLocalSigner(key), tradeModule: common.HexToAddress("0xa1"), matching: common.HexToAddress("0x9e"),
	}
	fixing := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	client.futureSchedule = &futureSchedule{fixingStart: fixing, lastTrade: fixing.Add(time.Hour)}
	req := PlaceOrderRequest{Market: "USDC-cNGN-31DEC26", Side: SideBuy, Price: 1400, Size: 1, OrderID: "mm:USDC-cNGN-31DEC26:b1", Nonce: "1"}
	if _, err := client.PlaceLimitOrder(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(posted) != 1 {
		t.Fatalf("posted %d orders", len(posted))
	}
	expiry, _ := posted[0]["expiry"].(float64)
	if int64(expiry) != fixing.Add(-time.Hour).Unix() {
		t.Fatalf("signed expiry %v, want the cutoff %v (a day was configured)", int64(expiry), fixing.Add(-time.Hour).Unix())
	}
	if posted[0]["worst_fee"] != "3000000000000000000" {
		t.Fatalf("worst_fee %v, want 3e18 a contract", posted[0]["worst_fee"])
	}

	client.futureSchedule = &futureSchedule{fixingStart: time.Now().UTC().Add(30 * time.Minute), lastTrade: time.Now().UTC().Add(time.Hour)}
	req.OrderID, req.Nonce = "mm:USDC-cNGN-31DEC26:b2", "2"
	if _, err := client.PlaceLimitOrder(context.Background(), req); err == nil || len(posted) != 1 {
		t.Fatalf("past the cutoff nothing is posted: %v, %d posted", err, len(posted))
	}
}
