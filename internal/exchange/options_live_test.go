package exchange

import (
	"context"
	"math"
	"math/big"
	"os"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/numofx/market-maker/internal/pricing"
)

// The quoter prices every strike the way the engine marks it. On Base mainnet, at one block, for each listed cNGN-30NOV26
// strike and side: the engine's getIsolatedMargin mark for 1M cNGN long, against Black-76 on the forward and vol the
// engine itself reads (via OptionInputs) and the block's timestamp. They must agree to a millionth.
//
// OPTIONS_LIVE_RPC_URL=<a Base mainnet RPC> go test ./internal/exchange -run Live -v. Skipped without it.
func TestOptionPricingMatchesTheEngineLive(t *testing.T) {
	url := os.Getenv("OPTIONS_LIVE_RPC_URL")
	if url == "" {
		t.Skip("set OPTIONS_LIVE_RPC_URL to a Base mainnet RPC")
	}
	ctx := context.Background()
	rpc, err := ethclient.DialContext(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer rpc.Close()
	head, err := rpc.BlockNumber(ctx)
	if err != nil {
		t.Fatal(err)
	}
	block := new(big.Int).SetUint64(head - 2)

	option := common.HexToAddress("0xA6a05dC25127CecbC048C7e6CBE35e1c8e503821")
	engine := common.HexToAddress("0x0705219a1594d5Fc5dC2573da4082cAf3e1B0950")
	vol := "0xf2db01cf76e11a83d8510dacdd69ab62b3bac5e0"
	size := new(big.Int).Mul(big.NewInt(1_000_000), decimalScale)

	worst := 0.0
	for _, strike := range []string{"0.00062", "0.00067", "0.00072", "0.00077", "0.00082"} {
		for _, call := range []bool{true, false} {
			wei, k, err := strikeWei(strike)
			if err != nil {
				t.Fatal(err)
			}
			terms := OptionTerms{Strike: k, StrikeWei: wei, Call: call, Expiry: 1796047200, VolFeed: vol}
			in, err := readOptionInputs(ctx, rpc, option, terms, block)
			if err != nil {
				t.Fatalf("%s: %v", strike, err)
			}
			_, mark, err := readEngineOptionMark(ctx, rpc, engine, option, terms, size, true, block)
			if err != nil {
				t.Fatalf("%s: %v", strike, err)
			}
			price := pricing.Option{Forward: in.Forward, Strike: k, Vol: in.Vol, Seconds: float64(terms.Expiry - in.BlockTime), Call: call}.Price()
			ours := price * 1_000_000
			rel := math.Abs(ours-mark) / math.Max(mark, 1e-12)
			worst = math.Max(worst, rel)
			t.Logf("%s %v: vol %.4f fwd %.12f | engine %.9f ours %.9f (rel %.2g)", strike, map[bool]string{true: "C", false: "P"}[call], in.Vol, in.Forward, mark, ours, rel)
			if rel > 1e-6 {
				t.Errorf("%s call=%v: Black-76 %.12f vs the engine's %.12f", strike, call, ours, mark)
			}
		}
	}
	t.Logf("worst relative difference %.2g at block %s", worst, block)
}
