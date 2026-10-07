package exchange

import (
	"context"
	"encoding/json"
	"math"
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
		{"USDCcNGN-SPOT", "spot", SpecCNGNUSDCSpot, MarketKindSpot},
		{"USDCcNGN-SPOT", "spot", SpecUSDCCNGNSpot, MarketKindSpot},
		{"USDCcNGN-SPOT", "", "", MarketKindSpot},
		{"USDCcNGN-PERP", "perpetual", SpecCNGNUSDCPerp, MarketKindPerp},
		{"USDCcNGN-PERP", "perpetual", SpecUSDCCNGNPerp, MarketKindPerp},
		{"USDCcNGN-PERP", "perpetual", "", MarketKindPerp},
		{"USDCcNGN-SEP16-2026", "future", "", MarketKindFuture},
	}
	for _, tc := range cases {
		if got := marketKind(tc.symbol, tc.contractType, tc.spec); got != tc.want {
			t.Fatalf("marketKind(%s, %s, %s) = %s, want %s", tc.symbol, tc.contractType, tc.spec, got, tc.want)
		}
	}
	// A spec built by hand classifies like a loaded one, under either presentation.
	for _, spec := range []string{SpecCNGNUSDCPerp, SpecUSDCCNGNPerp} {
		if !(MarketSpec{Symbol: "USDCcNGN-PERP", OrderEntrySpec: spec}).CNGNDenominated() {
			t.Fatalf("a perp spec (%s) must be cNGN-denominated", spec)
		}
	}
	if (MarketSpec{Symbol: "USDCcNGN-SEP16-2026"}).CNGNDenominated() {
		t.Fatal("a future is not cNGN-denominated")
	}
	if usesContractLots(MarketSpec{Symbol: "USDCcNGN-PERP", Kind: MarketKindPerp, SizeStep: 1}) {
		t.Fatal("the perp is not sized in contract lots")
	}
}

// perpRow is a /v1/markets perp row under one presentation. The engine fields (mark_price,
// index_price) are included only when withEngine is set, so both read paths are covered.
func perpRow(spec, base, quote, markUI, indexUI string, withEngine bool) map[string]any {
	perp := map[string]any{
		"mark_price_ui":          markUI,
		"index_price_ui":         indexUI,
		"open_interest":          "10000000",
		"max_leverage":           "3.0000300003",
		"trade_module_address":   "0xa708AC654e5eD2e980Bc7868f6663E68209dbDD0",
		"quote_asset_address":    "0x76b3dB736C7503544c492e20322dEda3DD6B828F",
		"margin_manager_address": "0x5dca1D15c325A5693BB07B1D0ADD01a887204deb",
		"trading_enabled":        true,
		"position_cap":           "50000000",
	}
	if withEngine {
		perp["mark_price"] = "0.000727802037845705"
		perp["index_price"] = "0.000727802037845705"
	}
	return map[string]any{
		"market":             "USDCcNGN-PERP",
		"base_asset_symbol":  base,
		"quote_asset_symbol": quote,
		"asset_address":      "0x5A8d6042B0F36a5b4BF30894F16E558E46D5f922",
		"sub_id":             "0",
		"tick_size":          "0.000000000000000001",
		"contract_type":      "perpetual",
		"order_entry_spec":   spec,
		"taker_fee_bps":      25,
		"perp":               perp,
	}
}

func loadPerpSpec(t *testing.T, row map[string]any) MarketSpec {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{row})
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
	return spec
}

// The same perp, read under each presentation the venue has served, must come out identical:
// cNGN the base, USDC the quote, the index in USDC per cNGN, sizes in whole cNGN.
func TestLoadMarketsReadsThePerpObjectUnderBothPresentations(t *testing.T) {
	const index = 1 / 1374.0
	cases := []struct {
		name string
		row  map[string]any
		want Orientation
	}{
		{"cngn_usdc (engine presentation, ui fields only)", perpRow(SpecCNGNUSDCPerp, "cNGN", "USDC", "0.000727802", "0.000727802", false), OrientationEngine},
		{"usdc_cngn (inverted presentation, ui fields only)", perpRow(SpecUSDCCNGNPerp, "USDC", "cNGN", "1375", "1374", false), OrientationInverted},
		{"usdc_cngn with engine fields", perpRow(SpecUSDCCNGNPerp, "USDC", "cNGN", "1375", "1374", true), OrientationInverted},
		{"unknown spec, engine assumed", perpRow("cngn_usdc_perp_v2", "cNGN", "USDC", "0.000727802", "0.000727802", false), OrientationEngine},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := loadPerpSpec(t, tc.row)
			if spec.Orientation != tc.want {
				t.Fatalf("orientation = %s, want %s", spec.Orientation, tc.want)
			}
			if spec.BaseAsset != "cNGN" || spec.QuoteAsset != "USDC" {
				t.Fatalf("assets = %s/%s, want cNGN/USDC in engine order", spec.BaseAsset, spec.QuoteAsset)
			}
			if spec.SizeStep != 1 || spec.MinSize != 1 {
				t.Fatalf("size step/min = %v/%v, want whole cNGN", spec.SizeStep, spec.MinSize)
			}
			p := spec.Perp
			if math.Abs(p.IndexPrice-index)/index > 1e-5 || p.MarkPrice <= 0 {
				t.Fatalf("index %v mark %v, want ~%v USDC per cNGN", p.IndexPrice, p.MarkPrice, index)
			}
			if !p.TradingEnabled || p.PositionCapNGN != 50_000_000 || p.MarginManager != "0x5dca1d15c325a5693bb07b1d0add01a887204deb" {
				t.Fatalf("perp state = %+v", p)
			}
			// 25M cNGN a side under the cap, 10M open: 15M cNGN of room, ~$10,917 at 1374.
			if room := p.SideRoomNGN(); room != 15_000_000 {
				t.Fatalf("side room = %v cNGN", room)
			}
			if room := p.SideRoomUSD(); room < 10_916 || room > 10_918 {
				t.Fatalf("side room = %v USD", room)
			}
		})
	}
}

// The engine holds the perp in cNGN, long cNGN positive, and so does the bot now: the position is
// reported as the engine's own signed cNGN amount, valued in USDC only for the log line.
func TestPerpBalancesReportTheEnginePosition(t *testing.T) {
	spec := MarketSpec{
		Kind: MarketKindPerp, Symbol: "USDCcNGN-PERP", BaseAsset: "cNGN", QuoteAsset: "USDC",
		AssetAddress: "0xperp", SubID: "0", QuoteAddress: "0xcash",
		Perp: &PerpState{IndexPrice: 1 / 1374.0},
	}
	balances, err := PerpBalances(spec, map[string]float64{"0xperp|0": 1_374_000, "0xcash|0": 5_000})
	if err != nil {
		t.Fatalf("PerpBalances: %v", err)
	}
	if balances[0].Asset != "cNGN" || balances[0].Total != 1_374_000 || balances[0].Available != 1_374_000 {
		t.Fatalf("position = %+v, want +1,374,000 cNGN (long cNGN)", balances[0])
	}
	if balances[1].Asset != "USDC" || balances[1].Total != 5_000 || balances[1].Reserved != 0 {
		t.Fatalf("cash = %+v", balances[1])
	}
	short, _ := PerpBalances(spec, map[string]float64{"0xperp|0": -500, "0xcash|0": 5_000})
	if short[0].Total != -500 {
		t.Fatalf("short position = %+v, want -500 cNGN", short[0])
	}
	if _, err := PerpBalances(MarketSpec{Kind: MarketKindPerp}, nil); err == nil {
		t.Fatal("a perp without an index must not value its position")
	}
}
