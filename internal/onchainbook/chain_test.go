package onchainbook

import (
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
)

func ids(methods map[string]abi.Method) map[string][]byte {
	out := map[string][]byte{}
	for name, m := range methods {
		out[name] = m.ID
	}
	return out
}

// Every selector this package sends or reads, pinned to `cast sig` (2026-11-16, exchange 01b7459; v2 entries
// 2026-11-17, exchange 6313ed0). A wrong one
// fails silently on chain -- the call reverts or reads empty space -- so the ABI copies are checked here.
func TestSelectorsMatchCastSig(t *testing.T) {
	a, err := loadABIs()
	if err != nil {
		t.Fatal(err)
	}
	pinned := []struct {
		abi    string
		method string
		want   string
	}{
		{"bookV1", "updateSplines", "df20ee06"}, {"book", "updateSplines", "5c06631f"},
		{"book", "isSelfPriced", "664b3f89"}, {"book", "maxSelfPricedTtl", "3a1359b6"},
		{"bookV1", "splines", "e5dd8989"}, {"bookV1", "markets", "b1283e77"}, {"book", "cancelSpline", "28dfa398"}, {"book", "setCurve", "3d3fc497"},
		{"book", "registerMaker", "733dad44"}, {"book", "splines", "e5dd8989"}, {"book", "curve", "b19f714d"},
		{"book", "isRegistered", "a5ff2665"}, {"book", "markets", "b1283e77"}, {"book", "makerAllowed", "4b3df9c5"},
		{"book", "subAccounts", "779e5012"},
		{"forward", "getForwardPrice", "b34f2e36"}, {"spot", "getSpot", "2b37269c"},
		{"engine", "getMarketFeeds", "a95371a4"}, {"subAccounts", "ownerOf", "6352211e"},
		{"subAccounts", "manager", "52981457"}, {"subAccounts", "positiveAssetAllowance", "4997e514"},
		{"subAccounts", "negativeAssetAllowance", "5d13e0eb"}, {"subAccounts", "getBalance", "0806e640"},
		{"future", "markIndexBand", "968564c5"},
	}
	byABI := map[string]map[string][]byte{
		"book": ids(a.book.Methods), "bookV1": ids(a.bookV1.Methods), "forward": ids(a.forward.Methods), "spot": ids(a.spot.Methods),
		"engine": ids(a.engine.Methods), "subAccounts": ids(a.subAccounts.Methods), "future": ids(a.future.Methods),
	}
	for _, p := range pinned {
		m, ok := byABI[p.abi][p.method]
		if !ok {
			t.Errorf("%s.%s missing from the ABI copy", p.abi, p.method)
			continue
		}
		if got := hex.EncodeToString(m); got != p.want {
			t.Errorf("%s.%s: %s, cast sig says %s", p.abi, p.method, got, p.want)
		}
	}
	if e, ok := a.book.Errors["OB_StaleSeq"]; !ok || hex.EncodeToString(e.ID[:4]) != "7d258704" {
		t.Error("OB_StaleSeq is not decodable by name")
	}
}

// The calldata for one spline update, byte for byte against
// cast calldata "updateSplines((uint256,uint256,int32,uint32,uint16,uint16,uint32,uint32,uint32,uint32,uint64)[])"
// "[(1,57,-495,100,1,1,2,200,50,3600,1794840000000)]": a negative offset is the case a packing slip would get wrong.
func TestUpdateCalldataMatchesCast(t *testing.T) {
	a, err := loadABIs()
	if err != nil {
		t.Fatal(err)
	}
	c := &Chain{a: a, MarketID: big.NewInt(1), AccountID: big.NewInt(57)}
	got, err := c.UpdateCalldata(Spline{OffsetTicks: -495, HalfSpreadTicks: 100, BidCurve: 1, AskCurve: 1,
		SkewTicksPerLot: 2, MaxSkewTicks: 200, RefillLotsPerSecond: 50, TTL: 3600, Seq: 1794840000000})
	if err != nil {
		t.Fatal(err)
	}
	const want = "df20ee06" +
		"0000000000000000000000000000000000000000000000000000000000000020" +
		"0000000000000000000000000000000000000000000000000000000000000001" +
		"0000000000000000000000000000000000000000000000000000000000000001" +
		"0000000000000000000000000000000000000000000000000000000000000039" +
		"fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffe11" +
		"0000000000000000000000000000000000000000000000000000000000000064" +
		"0000000000000000000000000000000000000000000000000000000000000001" +
		"0000000000000000000000000000000000000000000000000000000000000001" +
		"0000000000000000000000000000000000000000000000000000000000000002" +
		"00000000000000000000000000000000000000000000000000000000000000c8" +
		"0000000000000000000000000000000000000000000000000000000000000032" +
		"0000000000000000000000000000000000000000000000000000000000000e10" +
		"000000000000000000000000000000000000000000000000000001a1e4ccf600"
	if hex.EncodeToString(got) != want {
		t.Fatalf("calldata\n got %x\nwant %s", got, want)
	}
}

func TestSetCurvePacksRegions(t *testing.T) {
	a, err := loadABIs()
	if err != nil {
		t.Fatal(err)
	}
	data, err := a.book.Pack("setCurve", uint16(1), []Region{{Lots: 200, OffsetTicks: 0}, {Lots: 400, OffsetTicks: 50}})
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(data[:4]) != "3d3fc497" || len(data) != 4+32*7 {
		t.Fatalf("setCurve calldata %x", data)
	}
}
