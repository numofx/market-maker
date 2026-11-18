package onchainbook

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/ethclient"
)

func TestHTTPReference(t *testing.T) {
	now := time.Unix(1_795_041_200, 0)
	cases := []struct {
		name, body string
		status     int
		want       string // price, or an error substring prefixed with "err:"
	}{
		{"fresh", `{"usd_per_base_e18":"691000000000000","observed_at":1795041138}`, 200, "691000000000000"},
		{"stale", `{"usd_per_base_e18":"691000000000000","observed_at":1795040000}`, 200, "err:old"},
		{"no time", `{"usd_per_base_e18":"691000000000000"}`, 200, "err:old"},
		{"zero", `{"usd_per_base_e18":"0","observed_at":1795041138}`, 200, "err:positive"},
		{"garbage price", `{"usd_per_base_e18":"0.00069","observed_at":1795041138}`, 200, "err:positive"},
		{"not json", `rate limited`, 200, "err:body"},
		{"503", `{"error":"stale"}`, 503, "err:503"},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(c.body))
		}))
		r := &HTTPReference{URL: srv.URL, MaxAge: 5 * time.Minute, now: func() time.Time { return now }}
		got, err := r.Price(context.Background())
		srv.Close()
		if strings.HasPrefix(c.want, "err:") {
			if err == nil || !strings.Contains(err.Error(), strings.TrimPrefix(c.want, "err:")) {
				t.Errorf("%s: want an error with %q, got %v, %v", c.name, c.want, got, err)
			}
			continue
		}
		if err != nil || got.String() != c.want {
			t.Errorf("%s: want %s, got %v, %v", c.name, c.want, got, err)
		}
	}
}

// A dead rate service is a nil reference in Prices' output, so Decide takes the spline down; it never falls back to
// the engine's feed, which for cNGN is the index itself.
func TestDeadRateServiceTakesTheSplineDown(t *testing.T) {
	r := &HTTPReference{URL: "http://127.0.0.1:1/unreachable", MaxAge: time.Minute}
	if p, err := r.Price(context.Background()); err == nil || p != nil {
		t.Fatalf("an unreachable service must be an error, got %v", p)
	}
}

// With HTTPReference set, ResolveFeeds never asks the engine for a reference feed (for cNGN it would be the index
// itself) and Prices takes the reference from the rate service; a dead service leaves it nil.
func TestChainUsesTheHTTPReference(t *testing.T) {
	a, err := loadABIs()
	if err != nil {
		t.Fatal(err)
	}
	index := common.HexToAddress("0xFaC420d160C7c219A72DC676971670980bAC20a5")
	forward := common.HexToAddress("0x24474D8A95EB8Be9070ede58E09405B410c277Df")
	sel := func(x abi.ABI, m string) string { return hexutil.Encode(x.Methods[m].ID) }
	word := func(v *big.Int) string { return hexutil.Encode(common.LeftPadBytes(v.Bytes(), 32)) }
	answers := map[string]string{
		sel(a.future, "indexFeed"):        hexutil.Encode(common.LeftPadBytes(index.Bytes(), 32)),
		sel(a.future, "forwardFeed"):      hexutil.Encode(common.LeftPadBytes(forward.Bytes(), 32)),
		sel(a.spot, "getSpot"):            word(big.NewInt(687_349_999_516_984)) + strings.TrimPrefix(word(big.NewInt(1e18)), "0x"),
		sel(a.forward, "getForwardPrice"): word(big.NewInt(684_769_919_638_773)) + strings.TrimPrefix(word(big.NewInt(1e18)), "0x"),
	}
	engineFeeds := sel(a.engine, "getMarketFeeds")
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage   `json:"id"`
			Params []json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var msg struct{ Input, Data string }
		_ = json.Unmarshal(req.Params[0], &msg)
		data := msg.Input + msg.Data
		if strings.HasPrefix(data, engineFeeds) {
			t.Errorf("the engine's reference feed was read with an HTTP reference set")
		}
		out, ok := answers[data[:10]]
		if !ok {
			out = "0x"
		}
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%q}`, req.ID, out)
	}))
	defer node.Close()
	rate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"usd_per_base_e18":"691000000000000","observed_at":%d}`, time.Now().Unix())
	}))
	eth, err := ethclient.Dial(node.URL)
	if err != nil {
		t.Fatal(err)
	}
	c := &Chain{eth: eth, a: a, HTTPReference: &HTTPReference{URL: rate.URL, MaxAge: time.Minute}}
	feeds, err := c.ResolveFeeds(context.Background(), Market{Future: common.HexToAddress("0xcace"), Engine: common.HexToAddress("0x0705")}, 1)
	if err != nil {
		t.Fatal(err)
	}
	idx, fwd, ref, errs := c.Prices(context.Background(), feeds, 1796047200)
	if len(errs) > 0 || idx == nil || fwd == nil || ref == nil || ref.String() != "691000000000000" {
		t.Fatalf("index %v forward %v reference %v errs %v", idx, fwd, ref, errs)
	}
	rate.Close()
	if _, _, ref, errs = c.Prices(context.Background(), feeds, 1796047200); ref != nil || len(errs) != 1 {
		t.Fatalf("a dead rate service must leave the reference nil with one error, got %v %v", ref, errs)
	}
}
