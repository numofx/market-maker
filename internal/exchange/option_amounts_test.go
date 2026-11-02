package exchange

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

func optionSpec() MarketSpec {
	return MarketSpec{Symbol: "cNGN-30NOV26-0.00072-P", Kind: MarketKindOption, BaseAsset: "cNGN", AmountScale: "1000000000000000000000",
		SizeStep: 1000, MinSize: 1000, Option: &OptionTerms{Strike: 0.00072, Expiry: 1796047200, LastTradeTime: 1796043600, UnitBase: 1000}}
}

// The body counts 1,000-cNGN units and the signed amount is units x 1e21: markets-service refuses any other scale, and
// signing 1e18 per unit (the futures path) would have been refused at intake.
func TestOptionOrderAmounts(t *testing.T) {
	for _, tt := range []struct {
		size   float64
		body   string
		signed string
		round  float64
	}{
		{1000, "1", "1000000000000000000000", 1000},
		{100_000, "100", "100000000000000000000000", 100_000},
		{2999.9999999, "3", "3000000000000000000000", 3000}, // a float a hair under a unit is that unit
		{2500, "2", "2000000000000000000000", 2000},
	} {
		body, signed, rounded, err := optionOrderAmounts(optionSpec(), tt.size)
		if err != nil || body != tt.body || signed != tt.signed || rounded != tt.round {
			t.Fatalf("%v cNGN: body %s signed %s rounded %v (%v)", tt.size, body, signed, rounded, err)
		}
	}
	if _, _, _, err := optionOrderAmounts(optionSpec(), 999); err == nil {
		t.Fatal("less than one unit was signed")
	}
	noUnit := optionSpec()
	noUnit.Option.UnitBase = 0
	if _, _, _, err := optionOrderAmounts(noUnit, 1000); err == nil {
		t.Fatal("an option without a unit was signed")
	}
}

// The venue's unit counts read back as cNGN, so inventory (the engine's 1e18 per cNGN) and orders share one unit.
func TestOptionAmountsReadBackInBaseUnits(t *testing.T) {
	if got := orderAmountToFloat(optionSpec(), "100"); got != 100_000 {
		t.Fatalf("100 units read back as %v cNGN", got)
	}
	if got := rawOrderSizeToFloat(optionSpec(), "3"); got != 3000 {
		t.Fatalf("a 3-unit trade read back as %v cNGN", got)
	}
}

func TestOptionOrderExpiryStopsAtLastTrade(t *testing.T) {
	spec := optionSpec()
	now := time.Unix(1796043600-120, 0)
	got, err := cappedOrderExpiry(spec, now.Unix()+3600, time.Time{}, now)
	if err != nil || got != 1796043600 {
		t.Fatalf("expiry %d (%v), want the last trade time", got, err)
	}
	if _, err := cappedOrderExpiry(spec, 0, time.Time{}, time.Unix(1796043600, 0)); err == nil {
		t.Fatal("an order was signed at last trade")
	}
	spec.Option.LastTradeTime = 0
	if _, err := cappedOrderExpiry(spec, now.Unix()+60, time.Time{}, now); err == nil {
		t.Fatal("an order was signed with no last trade time")
	}
}

func TestOptionListingClassifiesAsOptionNotFuture(t *testing.T) {
	if k := marketKind("cNGN-30NOV26-0.00072-C", "option", ""); k != MarketKindOption {
		t.Fatalf("kind %s", k)
	}
	if optionSpec().IsMarginEngineFuture() || optionSpec().BaseSized() {
		t.Fatal("an option took a future's or spot's path")
	}
}

// The wiring: a real PlaceLimitOrder on an option listing, against a stub venue, sends desired_amount in 1,000-cNGN units,
// signs units x 1e21, caps the signed expiry at last trade, and bounds the fee per cNGN at the premium's bps.
func TestPlacingAnOptionSignsUnitsAtTheListedScale(t *testing.T) {
	var posted []map[string]any
	lastTrade := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second).Unix()
	venue := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"market": "cNGN-30NOV26-0.00072-P", "contract_type": "option", "base_asset_symbol": "cNGN", "quote_asset_symbol": "USDC",
				"asset_address": "0xa6a05dc25127cecbc048c7e6cbe35e1c8e503821", "sub_id": "39614081257132478036213334368",
				"expiry_timestamp": lastTrade + 3600, "tick_size": "0.000000000000000001", "amount_step": "1", "taker_fee_bps": 15,
				"amount_scale": "1000000000000000000000", "margin_manager_address": "0x0705219a1594d5fc5dc2573da4082caf3e1b0950",
				"option": map[string]any{"strike": "0.00072", "option_type": "put", "expiry_timestamp": lastTrade + 3600,
					"last_trade_time": lastTrade, "vol_feed_address": "0xf2db01cf76e11a83d8510dacdd69ab62b3bac5e0"},
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
			ChainID: 8453, OrderExpirySeconds: 86400, WorstFee: "0"},
		httpClient: &http.Client{Timeout: 5 * time.Second}, markets: make(map[string]MarketSpec), refreshEvery: time.Minute,
		signer: NewLocalSigner(key), tradeModule: common.HexToAddress("0xa1"), matching: common.HexToAddress("0x9e"),
	}
	req := PlaceOrderRequest{Market: "cNGN-30NOV26-0.00072-P", Side: SideSell, Price: 0.00003, Size: 100_000, OrderID: "mm:opt:a1", Nonce: "1"}
	order, err := client.PlaceLimitOrder(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(posted) != 1 || posted[0]["desired_amount"] != "100" {
		t.Fatalf("posted %v: want desired_amount 100 (units of 1,000 cNGN)", posted)
	}
	action, _ := posted[0]["action_json"].(map[string]any)
	data, _ := action["data"].(string)
	words := strings.TrimPrefix(data, "0x")
	if len(words) < 7*64 {
		t.Fatalf("action data %q", data)
	}
	amount, _ := new(big.Int).SetString(words[3*64:4*64], 16)
	if amount.String() != "100000000000000000000000" {
		t.Fatalf("signed desiredAmount %s, want 100 x 1e21", amount)
	}
	worstFee, _ := new(big.Int).SetString(words[4*64:5*64], 16)
	perCNGN := 0.00003 * float64(15+worstFeeHeadroomBps) / 10_000 * 1e18
	if f, _ := new(big.Float).SetInt(worstFee).Float64(); f < perCNGN*0.9999 || f > perCNGN*1.0001 {
		t.Fatalf("worstFee %s per cNGN, want ~%.0f (premium x %d bps)", worstFee, perCNGN, 15+worstFeeHeadroomBps)
	}
	if expiry, _ := action["expiry"].(string); expiry != strconv.FormatInt(lastTrade, 10) {
		t.Fatalf("signed expiry %s, want last trade %d", expiry, lastTrade)
	}
	if order.Size != 100_000 {
		t.Fatalf("the order reads back as %v cNGN", order.Size)
	}
}

func TestOptionWiringRefusesAnotherStack(t *testing.T) {
	spec := optionSpec()
	spec.Option.VolFeed, spec.MarginManager = "0xf2", "0x0705"
	spec.ListedTradeModule, spec.ListedQuoteAsset = "0xdea9", "0xa74e"
	if err := optionWiringError(spec, "0xDEA9", "0xA74E"); err != nil {
		t.Fatal(err)
	}
	if optionWiringError(spec, "0xbeef", "0xa74e") == nil || optionWiringError(spec, "0xdea9", "0xbeef") == nil {
		t.Fatal("another trade module or quote asset was accepted")
	}
	spec.ListedTradeModule = ""
	if optionWiringError(spec, "0xdea9", "0xa74e") == nil {
		t.Fatal("a listing without a trade module was accepted")
	}
}
