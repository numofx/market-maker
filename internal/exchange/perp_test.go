package exchange

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMarketKind(t *testing.T) {
	cases := []struct {
		symbol, contractType, spec string
		want                       MarketKind
	}{
		{"USDCcNGN-SPOT", "spot", "usdc_cngn_spot_v1", MarketKindSpot},
		{"USDCcNGN-SPOT", "", "", MarketKindSpot},
		{"USDCcNGN-PERP", "perpetual", "usdc_cngn_perp_v1", MarketKindPerp},
		{"USDCcNGN-PERP", "perpetual", "", MarketKindPerp},
		{"USDCcNGN-SEP16-2026", "future", "", MarketKindFuture},
	}
	for _, tc := range cases {
		if got := marketKind(tc.symbol, tc.contractType, tc.spec); got != tc.want {
			t.Fatalf("marketKind(%s, %s, %s) = %s, want %s", tc.symbol, tc.contractType, tc.spec, got, tc.want)
		}
	}
	// A spec built by hand classifies like a loaded one.
	if !(MarketSpec{Symbol: "USDCcNGN-PERP", OrderEntrySpec: "usdc_cngn_perp_v1"}).UIInverted() {
		t.Fatal("a perp spec must be UI-inverted")
	}
	if usesContractLots(MarketSpec{Symbol: "USDCcNGN-PERP", Kind: MarketKindPerp, SizeStep: 0.000001}) {
		t.Fatal("the perp is not sized in contract lots")
	}
}

func TestLoadMarketsReadsThePerpObject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"market":             "USDCcNGN-PERP",
			"base_asset_symbol":  "USDC",
			"quote_asset_symbol": "cNGN",
			"asset_address":      "0x5A8d6042B0F36a5b4BF30894F16E558E46D5f922",
			"sub_id":             "0",
			"tick_size":          "0.000000000000000001",
			"contract_type":      "perpetual",
			"order_entry_spec":   "usdc_cngn_perp_v1",
			"taker_fee_bps":      25,
			"perp": map[string]any{
				"mark_price_ui":          "1375",
				"index_price_ui":         "1374",
				"open_interest":          "10000000",
				"max_leverage":           "3.0000300003",
				"trade_module_address":   "0xa708AC654e5eD2e980Bc7868f6663E68209dbDD0",
				"quote_asset_address":    "0x76b3dB736C7503544c492e20322dEda3DD6B828F",
				"margin_manager_address": "0x5dca1D15c325A5693BB07B1D0ADD01a887204deb",
				"trading_enabled":        true,
				"position_cap":           "50000000",
			},
		}})
	}))
	t.Cleanup(srv.Close)
	client := &HTTPClient{
		cfg:          ClientConfig{APIBaseURL: srv.URL, MarketSymbol: "USDCcNGN-PERP", WorstFee: "0"},
		httpClient:   &http.Client{Timeout: 5 * time.Second},
		markets:      make(map[string]MarketSpec),
		refreshEvery: time.Minute,
	}
	spec, err := client.GetMarket(context.Background(), "USDCcNGN-PERP")
	if err != nil {
		t.Fatalf("GetMarket: %v", err)
	}
	if !spec.IsPerp() || spec.Perp == nil {
		t.Fatalf("spec = %+v, want a perp with state", spec)
	}
	if spec.SizeStep != 0.000001 {
		t.Fatalf("size step %v, want spot's 0.000001 (whole NGN engine units)", spec.SizeStep)
	}
	p := spec.Perp
	if p.IndexPriceUI != 1374 || !p.TradingEnabled || p.PositionCapNGN != 50_000_000 || p.MarginManager != "0x5dca1d15c325a5693bb07b1d0add01a887204deb" {
		t.Fatalf("perp state = %+v", p)
	}
	// 25M NGN a side under the cap, 10M open: 15M NGN of room, $10,917 at 1374.
	if room := p.SideRoomUSD(); room < 10_916 || room > 10_918 {
		t.Fatalf("side room = %v", room)
	}
}

// The engine holds the perp in NGN, long NGN positive; the bot reads a UI position in USD, long USD
// positive. A NGN long is therefore a negative UI position.
func TestPerpBalancesFlipAndValueThePosition(t *testing.T) {
	spec := MarketSpec{
		Kind: MarketKindPerp, Symbol: "USDCcNGN-PERP", BaseAsset: "USDC", QuoteAsset: "cNGN",
		AssetAddress: "0xperp", SubID: "0", QuoteAddress: "0xcash",
		Perp: &PerpState{IndexPriceUI: 1374},
	}
	balances, err := PerpBalances(spec, map[string]float64{"0xperp|0": 1_374_000, "0xcash|0": 5_000})
	if err != nil {
		t.Fatalf("perpBalances: %v", err)
	}
	if balances[0].Asset != "USDC" || math_abs(balances[0].Total-(-1_000)) > 1e-9 {
		t.Fatalf("position = %+v, want -$1,000 (a UI short)", balances[0])
	}
	if balances[1].Asset != "cNGN" || balances[1].Total != 5_000 || balances[1].Reserved != 0 {
		t.Fatalf("cash = %+v", balances[1])
	}
	if _, err := PerpBalances(MarketSpec{Kind: MarketKindPerp}, nil); err == nil {
		t.Fatal("a perp without an index must not value its position")
	}
}

func math_abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
