package exchange

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

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
