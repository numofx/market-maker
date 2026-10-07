package exchange

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func within(got, want, rel float64) bool {
	if want == 0 {
		return got == 0
	}
	return math.Abs(got-want)/math.Abs(want) <= rel
}

func TestOrientationForSpec(t *testing.T) {
	for spec, want := range map[string]Orientation{
		SpecCNGNUSDCSpot: OrientationEngine,
		SpecCNGNUSDCPerp: OrientationEngine,
		SpecUSDCCNGNSpot: OrientationInverted,
		SpecUSDCCNGNPerp: OrientationInverted,
	} {
		got, ok := OrientationForSpec(spec)
		if !ok || got != want {
			t.Fatalf("OrientationForSpec(%s) = %s/%v, want %s", spec, got, ok, want)
		}
	}
	if got, ok := OrientationForSpec("something_else_v9"); ok || got != OrientationEngine {
		t.Fatalf("unknown spec = %s/%v, want engine and not known", got, ok)
	}
	if got, ok := OrientationForSpec(""); ok || got != OrientationEngine {
		t.Fatalf("empty spec = %s/%v, want engine and not known", got, ok)
	}
}

// The perp's index as production served it on 2026-10-07 under usdc_cngn (1363.422408 cNGN per
// USDC) and as the local venue served it under cngn_usdc (0.000727802 USDC per cNGN): the bot must
// read a USDC-per-cNGN number near 0.00073 from both.
func TestEnginePriceUnderBothPresentations(t *testing.T) {
	inverted, err := OrientationInverted.EnginePrice(1363.422408)
	if err != nil || !within(inverted, 0.000733448, 1e-6) {
		t.Fatalf("inverted 1363.422408 -> %v (%v), want ~0.000733448", inverted, err)
	}
	engine, err := OrientationEngine.EnginePrice(0.000727802)
	if err != nil || engine != 0.000727802 {
		t.Fatalf("engine 0.000727802 -> %v (%v), want unchanged", engine, err)
	}
	if _, err := OrientationInverted.EnginePrice(0); err == nil {
		t.Fatal("a zero price must not invert to +Inf")
	}
	// liquidation_price_ui goes through the same function: 1400 cNGN per USDC is 1/1400.
	if liq, _ := OrientationInverted.EnginePrice(1400); !within(liq, 1/1400.0, 1e-12) {
		t.Fatalf("liquidation 1400 -> %v", liq)
	}
}

// Rows taken verbatim from the two venues on 2026-10-07. Production's row is the spot maker's own
// resting order presented as a UI sell of 19.999673 USDC at 1365.772339; its engine_order on the
// same row says buy 27315 cNGN at 0.000732186449930284, and that is what must come out.
func TestEngineOrderFromUIIntentUnderBothPresentations(t *testing.T) {
	side, price, size, err := EngineOrderFromUIIntent(SpecUSDCCNGNSpot, "sell", "1365.772339", "19.999673")
	if err != nil {
		t.Fatalf("inverted row: %v", err)
	}
	if side != SideBuy || !within(price, 0.000732186449930284, 1e-8) || !within(size, 27315, 1e-5) {
		t.Fatalf("inverted row -> %s %v x %v, want buy 0.000732186 x 27315", side, price, size)
	}
	side, price, size, err = EngineOrderFromUIIntent(SpecUSDCCNGNPerp, "sell", "1360.530279", "41.510285")
	if err != nil || side != SideBuy || !within(price, 0.00073500753010437, 1e-8) || !within(size, 56476, 1e-5) {
		t.Fatalf("inverted perp trade -> %s %v x %v (%v), want buy 0.000735008 x 56476", side, price, size, err)
	}
	side, price, size, err = EngineOrderFromUIIntent(SpecCNGNUSDCSpot, "buy", "0.000728", "68700")
	if err != nil || side != SideBuy || price != 0.000728 || size != 68700 {
		t.Fatalf("engine row -> %s %v x %v (%v), want buy 0.000728 x 68700 unchanged", side, price, size, err)
	}
	// An unknown spec is read as the engine's own presentation rather than refused.
	side, price, size, err = EngineOrderFromUIIntent("cngn_usdc_spot_v2", "sell", "0.000731", "1000")
	if err != nil || side != SideSell || price != 0.000731 || size != 1000 {
		t.Fatalf("unknown spec row -> %s %v x %v (%v)", side, price, size, err)
	}
	if _, _, _, err := EngineOrderFromUIIntent(SpecUSDCCNGNSpot, "buy", "abc", "1"); err == nil {
		t.Fatal("an unparseable price must error")
	}
	if _, _, _, err := EngineOrderFromUIIntent(SpecUSDCCNGNSpot, "hold", "1", "1"); err == nil {
		t.Fatal("an unknown side must error")
	}
}

func TestEngineSymbols(t *testing.T) {
	if b, q := OrientationInverted.EngineSymbols("USDC", "cNGN"); b != "cNGN" || q != "USDC" {
		t.Fatalf("inverted symbols = %s/%s", b, q)
	}
	if b, q := OrientationEngine.EngineSymbols("cNGN", "USDC"); b != "cNGN" || q != "USDC" {
		t.Fatalf("engine symbols = %s/%s", b, q)
	}
}

func TestPerpPriceFromVenuePrefersTheEngineFieldAndConvertsTheUIField(t *testing.T) {
	if got := perpPriceFromVenue("0.000733448411984", "1363.422408", OrientationInverted); got != 0.000733448411984 {
		t.Fatalf("engine field present: %v", got)
	}
	if got := perpPriceFromVenue("", "1363.422408", OrientationInverted); !within(got, 1/1363.422408, 1e-12) {
		t.Fatalf("inverted ui field only: %v", got)
	}
	if got := perpPriceFromVenue("", "0.000727802", OrientationEngine); got != 0.000727802 {
		t.Fatalf("engine ui field only: %v", got)
	}
	if got := perpPriceFromVenue("", "", OrientationInverted); got != 0 {
		t.Fatalf("nothing served: %v", got)
	}
}

// spotVenue serves /v1/markets, /v1/book and /v1/trades for USDCcNGN-SPOT under one presentation,
// with the engine fields markets-service carries on every row regardless of it.
func spotVenue(t *testing.T, spec, base, quote string) *httptest.Server {
	t.Helper()
	uiBid := map[string]any{"side": "buy", "price": "0.000728", "size": "68700"}
	uiAsk := map[string]any{"side": "sell", "price": "0.000735", "size": "27206"}
	if spec == SpecUSDCCNGNSpot {
		// The same two orders as production presents them: a UI sell of USDC and a UI buy of USDC.
		uiBid = map[string]any{"side": "sell", "price": "1373.626374", "size": "50.0136"}
		uiAsk = map[string]any{"side": "buy", "price": "1360.544218", "size": "19.99641"}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/markets"):
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"market": "USDCcNGN-SPOT", "contract_type": "spot",
				"base_asset_symbol": base, "quote_asset_symbol": quote,
				"asset_address": "0x9d806fd040a719d27a8e5e77dc5ae0ed1e089493", "sub_id": "0",
				"tick_size": "0.000000000000000001", "order_entry_spec": spec, "taker_fee_bps": 25,
			}})
		case strings.HasSuffix(r.URL.Path, "/v1/book"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"bids": []map[string]any{{"order_id": "b1", "side": "buy", "limit_price": "0.000728", "desired_amount": "68700",
					"spot_contract": map[string]any{"spec": spec, "ui_intent": uiBid}}},
				"asks": []map[string]any{{"order_id": "a1", "side": "sell", "limit_price": "0.000735", "desired_amount": "27206",
					"spot_contract": map[string]any{"spec": spec, "ui_intent": uiAsk}}},
			})
		case strings.HasSuffix(r.URL.Path, "/v1/trades"):
			_ = json.NewEncoder(w).Encode(map[string]any{"trades": []map[string]any{{
				"trade_id": 391, "price": "0.00073500753010437", "size": "56476", "aggressor_side": "buy",
				"created_at":    "2026-10-07T10:01:45.327555Z",
				"spot_contract": map[string]any{"spec": spec, "ui_intent": uiAsk},
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The book and the tape come out in engine terms under either presentation, from the engine fields
// every row carries: bids at ~0.000728 USDC per cNGN sized 68,700 cNGN, never 1373 x 50.
func TestBookAndTradesAreEngineOrientedUnderBothPresentations(t *testing.T) {
	cases := []struct {
		name, spec, base, quote string
	}{
		{"cngn_usdc", SpecCNGNUSDCSpot, "cNGN", "USDC"},
		{"usdc_cngn", SpecUSDCCNGNSpot, "USDC", "cNGN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := spotVenue(t, tc.spec, tc.base, tc.quote)
			c := newTestClient(t, srv.URL, time.Minute)
			spec, err := c.GetMarket(context.Background(), "USDCcNGN-SPOT")
			if err != nil {
				t.Fatalf("GetMarket: %v", err)
			}
			if spec.BaseAsset != "cNGN" || spec.QuoteAsset != "USDC" || spec.SizeStep != 1 {
				t.Fatalf("spec = base %s quote %s step %v, want cNGN/USDC/1", spec.BaseAsset, spec.QuoteAsset, spec.SizeStep)
			}
			book, err := c.GetBook(context.Background(), "USDCcNGN-SPOT")
			if err != nil {
				t.Fatalf("GetBook: %v", err)
			}
			if len(book.Bids) != 1 || len(book.Asks) != 1 {
				t.Fatalf("book = %+v, want one level a side", book)
			}
			if book.Bids[0].Price != 0.000728 || book.Bids[0].Size != 68700 || book.Bids[0].OrderID != "b1" {
				t.Fatalf("bid = %+v, want 0.000728 x 68700", book.Bids[0])
			}
			if book.Asks[0].Price != 0.000735 || book.Asks[0].Size != 27206 {
				t.Fatalf("ask = %+v, want 0.000735 x 27206", book.Asks[0])
			}
			trades, err := c.GetTrades(context.Background(), "USDCcNGN-SPOT")
			if err != nil || len(trades) != 1 {
				t.Fatalf("GetTrades: %v / %d", err, len(trades))
			}
			if trades[0].Price != 0.00073500753010437 || trades[0].Size != 56476 || trades[0].Side != SideBuy {
				t.Fatalf("trade = %+v, want buy 0.000735 x 56476", trades[0])
			}
		})
	}
}

// RequiredWorstFee takes the engine price. At the local venue's 0.000728 USDC per cNGN, 25 bps +
// 5 bps headroom per cNGN is 0.000728 x 0.0030 = 2.184e-6 USDC, i.e. 2184000000000 wei -- the
// exact worst_fee the venue's own maker signed on its 0.000728 orders (2026-10-07).
func TestRequiredWorstFeeAtAUSDCPerCNGNPrice(t *testing.T) {
	c := &HTTPClient{cfg: ClientConfig{WorstFee: "0"}}
	got, err := c.RequiredWorstFee(MarketSpec{Symbol: "USDCcNGN-SPOT", Kind: MarketKindSpot, TakerFeeBps: 25}, 0.000728)
	if err != nil {
		t.Fatalf("RequiredWorstFee: %v", err)
	}
	if got != "2184000000000" {
		t.Fatalf("bound = %s, want 2184000000000", got)
	}
	// A cNGN-per-USDC number here would be the old defect, ~1.8 million times too large.
	wrong, _ := c.RequiredWorstFee(MarketSpec{Symbol: "USDCcNGN-SPOT", Kind: MarketKindSpot, TakerFeeBps: 25}, 1373)
	if wrong == got {
		t.Fatal("the bound must depend on the price it is given")
	}
}
