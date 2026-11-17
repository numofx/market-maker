package onchainbook

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/ethclient"
)

func selfParams() Params {
	p := params()
	p.SelfPriced = true
	p.TTL = 10 * time.Second
	p.RefreshAfter = 2 * time.Second
	return p
}

// Self-priced (spec C4.2): the mid is the forward itself in ticks, not its basis to the index.
func TestSelfPricedSendsTheForwardAsTheMid(t *testing.T) {
	d := Decide(selfParams(), inputs(), Spline{})
	// 1.101093713422746282 / 1e-6 = 1,101,093.71 -> 1,101,094
	if d.Action != ActionUpdate || d.Target.OffsetTicks != 1_101_094 || !d.Target.SelfPriced || d.Target.TTL != 10 {
		t.Fatalf("decision %+v", d)
	}
}

// Every cycle refreshes a 10s quote: at 2s old it is due.
func TestSelfPricedRefreshesEveryCycle(t *testing.T) {
	p := selfParams()
	cur := Decide(p, inputs(), Spline{}).Target
	cur.UpdatedAt, cur.Owner = now, maker
	in := inputs()
	in.Now = now.Add(time.Second)
	if d := Decide(p, in, cur); d.Action != ActionNone {
		t.Fatalf("1s old: %+v", d)
	}
	in.Now = now.Add(2 * time.Second)
	if d := Decide(p, in, cur); d.Action != ActionUpdate || d.Reason != "refresh" {
		t.Fatalf("2s old: %+v", d)
	}
	in.Now = now.Add(11 * time.Second)
	if d := Decide(p, in, cur); d.Action != ActionUpdate || d.Reason != "down" {
		t.Fatalf("past the ttl it is down: %+v", d)
	}
}

// Switching mode is a configuration change in both directions, even when the number sent happens to agree.
func TestSwitchingModeIsAConfigChange(t *testing.T) {
	pegged := live(495, now)
	pegged.TTL = 10
	if d := Decide(selfParams(), inputs(), pegged); d.Reason != "config_changed" || !d.Target.SelfPriced {
		t.Fatalf("pegged -> self-priced: %+v", d)
	}
	self := live(495, now)
	self.SelfPriced = true
	if d := Decide(params(), inputs(), self); d.Reason != "config_changed" || d.Target.SelfPriced {
		t.Fatalf("self-priced -> pegged: %+v", d)
	}
}

// The guards hold in self-priced mode too: the book's band reads the index, so an unreadable index or a diverging one
// takes the quote down, though the mid does not use the index.
func TestSelfPricedStillTakesTheSplineDownOnTheIndex(t *testing.T) {
	cur := live(1_101_094, now)
	cur.SelfPriced, cur.TTL = true, 10
	in := inputs()
	in.Index = nil
	if d := Decide(selfParams(), in, cur); d.Action != ActionCancel || d.Reason != "index_unreadable" {
		t.Fatalf("index unreadable: %+v", d)
	}
	in = inputs()
	in.Reference = e18(1.2)
	if d := Decide(selfParams(), in, cur); d.Action != ActionCancel || d.Reason != "index_reference_divergence" {
		t.Fatalf("divergence: %+v", d)
	}
}

func TestMidTicksMustBePositive(t *testing.T) {
	if m, ok := MidTicks(e18(1.1), tick); !ok || m != 1_100_000 {
		t.Fatalf("1.1: %d %v", m, ok)
	}
	if _, ok := MidTicks(big.NewInt(4e11), tick); ok {
		t.Error("a mid that rounds to 0 is refused (OB_InvalidMid)")
	}
	if _, ok := MidTicks(e18(3000), tick); ok {
		t.Error("a mid past int32 is refused")
	}
	in := inputs()
	in.Forward = big.NewInt(4e11)
	if d := Decide(selfParams(), in, Spline{}); d.Action != ActionNone || d.Reason != "offset_out_of_range" {
		t.Fatalf("a zero mid is not sent: %+v", d)
	}
}

// Decision C6: ~10s ttl at a 5s cadence. Refresh 2s fits (2+5+3 = 10); refresh 3s would let the quote lapse.
func TestCheckCadence(t *testing.T) {
	p := selfParams()
	if err := CheckCadence(p, 5*time.Second); err != nil {
		t.Fatalf("10s ttl, 5s poll, 2s refresh: %v", err)
	}
	p.RefreshAfter = 3 * time.Second
	if err := CheckCadence(p, 5*time.Second); err == nil {
		t.Fatal("3s refresh at a 5s poll can lapse a 10s quote")
	}
	pegged := p // the same timings that fail above, pegged: exempt
	pegged.SelfPriced = false
	if err := CheckCadence(pegged, 5*time.Second); err != nil {
		t.Fatalf("pegged is not held to it: %v", err)
	}
}

// Byte for byte against
// cast calldata "updateSplines((uint256,uint256,int32,uint32,uint16,uint16,uint32,uint32,uint32,uint32,uint64,bool)[])"
// "[(1,57,1101094,100,1,1,2,200,50,10,1794840000000,true)]"
func TestV2UpdateCalldataMatchesCast(t *testing.T) {
	a, err := loadABIs()
	if err != nil {
		t.Fatal(err)
	}
	c := &Chain{a: a, MarketID: big.NewInt(1), AccountID: big.NewInt(57), V2: true}
	got, err := c.UpdateCalldata(Spline{OffsetTicks: 1_101_094, HalfSpreadTicks: 100, BidCurve: 1, AskCurve: 1,
		SkewTicksPerLot: 2, MaxSkewTicks: 200, RefillLotsPerSecond: 50, TTL: 10, Seq: 1794840000000, SelfPriced: true})
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(got) != strings.TrimPrefix(v2CastCalldata, "0x") {
		t.Fatalf("calldata\n got %x\nwant %s", got, v2CastCalldata)
	}
}

func TestV1RefusesASelfPricedSpline(t *testing.T) {
	a, err := loadABIs()
	if err != nil {
		t.Fatal(err)
	}
	c := &Chain{a: a, MarketID: big.NewInt(1), AccountID: big.NewInt(57)}
	if _, err := c.UpdateCalldata(Spline{OffsetTicks: 1_101_094, TTL: 10, SelfPriced: true}); !errors.Is(err, ErrSelfPricedOnV1) {
		t.Fatalf("v1 with a self-priced spline: %v", err)
	}
}

// The reads decode both versions with v1's flat outputs: v2's splines returns a struct of the same static fields
// (the same bytes), and markets is the same auto getter.
func TestV2ReadsHaveV1sLayout(t *testing.T) {
	a, err := loadABIs()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"splines", "markets"} {
		v1, v2 := a.bookV1.Methods[name], a.book.Methods[name]
		if hex.EncodeToString(v1.ID) != hex.EncodeToString(v2.ID) {
			t.Fatalf("%s: selector %x vs %x", name, v1.ID, v2.ID)
		}
		var flat2 []string
		for _, o := range v2.Outputs {
			if len(o.Type.TupleElems) > 0 {
				for _, e := range o.Type.TupleElems {
					flat2 = append(flat2, e.String())
				}
			} else {
				flat2 = append(flat2, o.Type.String())
			}
		}
		var flat1 []string
		for _, o := range v1.Outputs {
			flat1 = append(flat1, o.Type.String())
		}
		if strings.Join(flat1, ",") != strings.Join(flat2, ",") {
			t.Fatalf("%s: v1 %v, v2 %v", name, flat1, flat2)
		}
	}
}

// detectV2 against a JSON-RPC stub: an answer is v2, a bare revert (v1 has no such function and no fallback) is v1,
// and anything else (a custom revert, a dead endpoint) is an error rather than a guess.
func TestDetectV2(t *testing.T) {
	cases := []struct {
		name  string
		reply string
		v2    bool
		err   bool
	}{
		{"v2", `{"jsonrpc":"2.0","id":%ID%,"result":"0x000000000000000000000000000000000000000000000000000000000000003c"}`, true, false},
		{"v1, revert with empty data", `{"jsonrpc":"2.0","id":%ID%,"error":{"code":3,"message":"execution reverted","data":"0x"}}`, false, false},
		{"v1, revert without data", `{"jsonrpc":"2.0","id":%ID%,"error":{"code":-32000,"message":"execution reverted"}}`, false, false},
		{"a custom revert", `{"jsonrpc":"2.0","id":%ID%,"error":{"code":3,"message":"execution reverted","data":"0x7d258704"}}`, false, true},
		{"rate limited", `{"jsonrpc":"2.0","id":%ID%,"error":{"code":429,"message":"too many requests"}}`, false, true},
		{"a short answer", `{"jsonrpc":"2.0","id":%ID%,"result":"0x"}`, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				var req struct {
					ID json.RawMessage `json:"id"`
				}
				_ = json.Unmarshal(body, &req)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, strings.ReplaceAll(tc.reply, "%ID%", string(req.ID)))
			}))
			defer srv.Close()
			eth, err := ethclient.Dial(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			a, err := loadABIs()
			if err != nil {
				t.Fatal(err)
			}
			c := &Chain{eth: eth, a: a, MarketID: big.NewInt(1)}
			v2, err := c.detectV2(context.Background())
			if (err != nil) != tc.err || v2 != tc.v2 {
				t.Fatalf("v2=%v err=%v; want v2=%v err=%v", v2, err, tc.v2, tc.err)
			}
		})
	}
}

const v2CastCalldata = "0x5c06631f0000000000000000000000000000000000000000000000000000000000000020000000000000000000000000000000000000000000000000000000000000000100000000000000000000000000000000000000000000000000000000000000010000000000000000000000000000000000000000000000000000000000000039000000000000000000000000000000000000000000000000000000000010cd26000000000000000000000000000000000000000000000000000000000000006400000000000000000000000000000000000000000000000000000000000000010000000000000000000000000000000000000000000000000000000000000001000000000000000000000000000000000000000000000000000000000000000200000000000000000000000000000000000000000000000000000000000000c80000000000000000000000000000000000000000000000000000000000000032000000000000000000000000000000000000000000000000000000000000000a000000000000000000000000000000000000000000000000000001a1e4ccf6000000000000000000000000000000000000000000000000000000000000000001"
