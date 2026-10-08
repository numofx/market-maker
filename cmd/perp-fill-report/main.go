// perp-fill-report: a read-only report on the USDCcNGN-PERP market maker's account for a time window.
//
//	go run ./cmd/perp-fill-report -from 2026-10-05T14:00:00Z -to now
//	go run ./cmd/perp-fill-report -from 24h            # the last 24 hours
//
// It reads markets-service (/v1/trades) and the chain (SubAccounts BalanceAdjusted events, the index
// feed's SpotPriceUpdated events, the perp's unrealized cash) and writes nothing anywhere. It shows
// every fill the bot took part in with the index just before and after, flags fills followed within
// 60 s by an index move of more than 20 bps against the bot, the account's realized and unrealized
// P&L against its starting cash, and its inventory over time.
//
// Orientation is the engine's throughout: prices and the index in USDC per cNGN, sizes and the
// position in cNGN, a long is long cNGN. Fills are read from each trade's engine fields (price,
// size, aggressor_side), which markets-service serves identically under both of its public
// contracts; a row without them is decoded from its ui_intent through its own spot_contract.spec.
//
// Env: MM_RPC_URL (or -rpc), MM_API_BASE_URL (or -api, default https://api.numofx.com).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/numofx/market-maker/internal/exchange"

	"github.com/numofx/market-maker/internal/marketnames"
)

// Base mainnet defaults: the perp stack as deployed 2026-10-01 (numofx/exchange CNGN_PERP_STACK.json).
const (
	defaultAPI         = "https://api.numofx.com"
	defaultAccount     = 24
	defaultStartCash   = 4000.14
	defaultSubAccounts = "0x7019244E25FA416e6Ca2ed2F3cA25277aef72843"
	defaultPerp        = "0xC74EfC8B4808803dBCF439E76Fde076d56625b8E"
	defaultCash        = "0xA74E49b4Ed7cb176bc02ef4D8a1A3240C9aD4272"
	defaultIndexFeed   = "0xFaC420d160C7c219A72DC676971670980bAC20a5"
	adverseWindow      = 60 * time.Second
	adverseMoveBPS     = 20.0
	logChunk           = 5000
)

var e18 = new(big.Float).SetFloat64(1e18)

type trade struct {
	TradeID       int64  `json:"trade_id"`
	CreatedAt     string `json:"created_at"`
	MakerOrderID  string `json:"maker_order_id"`
	TakerOrderID  string `json:"taker_order_id"`
	Price         string `json:"price"`          // engine: USDC per cNGN
	Size          string `json:"size"`           // engine: cNGN
	AggressorSide string `json:"aggressor_side"` // engine side of the taker
	SpotContract  *struct {
		Spec     string `json:"spec"`
		UIIntent struct {
			Side  string `json:"side"`
			Price string `json:"price"`
			Size  string `json:"size"`
		} `json:"ui_intent"`
	} `json:"spot_contract"`
}

const (
	botBuysCNGN  = "buy cNGN"
	botSellsCNGN = "sell cNGN"
)

type fill struct {
	at      time.Time
	tradeID int64
	botSide string  // botBuysCNGN or botSellsCNGN
	size    float64 // cNGN
	price   float64 // USDC per cNGN
	role    string
}

type indexPoint struct {
	at    time.Time
	index float64 // USDC per cNGN
}

type balancePoint struct {
	at    time.Time
	block uint64
	perp  *float64
	cash  *float64
}

func main() {
	fromFlag := flag.String("from", "24h", "window start: RFC3339, or a duration back from now (e.g. 24h)")
	toFlag := flag.String("to", "now", "window end: RFC3339 or now")
	api := flag.String("api", envOr("MM_API_BASE_URL", defaultAPI), "markets-service base URL")
	rpcURL := flag.String("rpc", os.Getenv("MM_RPC_URL"), "Base RPC URL (default MM_RPC_URL)")
	account := flag.Int64("account", defaultAccount, "the market maker's subaccount id")
	startCash := flag.Float64("start-cash", defaultStartCash, "cash the account started with, USDC")
	flag.Parse()
	if *rpcURL == "" {
		fatalf("MM_RPC_URL or -rpc is required")
	}
	from, to, err := parseWindow(*fromFlag, *toFlag)
	if err != nil {
		fatalf("window: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	client, err := ethclient.DialContext(ctx, *rpcURL)
	if err != nil {
		fatalf("rpc: %v", err)
	}

	fmt.Printf("cNGN-PERP market maker #%d, %s to %s\n\n", *account, from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))

	fromBlock, err := blockAtOrAfter(ctx, client, from.Add(-2*time.Hour)) // index history before the first fill
	if err != nil {
		fatalf("block for window start: %v", err)
	}
	toBlock, err := blockAtOrAfter(ctx, client, to)
	if err != nil {
		fatalf("block for window end: %v", err)
	}

	indexes, err := readIndexHistory(ctx, client, common.HexToAddress(defaultIndexFeed), fromBlock, toBlock)
	if err != nil {
		fatalf("index history: %v", err)
	}
	fills, err := readFills(ctx, *api, from, to)
	if err != nil {
		fatalf("fills: %v", err)
	}
	balances, err := readBalanceHistory(ctx, client, common.HexToAddress(defaultSubAccounts), *account, fromBlock, toBlock)
	if err != nil {
		fatalf("balance history: %v", err)
	}

	index, err := currentIndex(ctx, client)
	if err != nil {
		fatalf("index: %v", err)
	}
	printFills(fills, indexes)
	printInventory(balances, from, index)
	if err := printPnL(ctx, client, *account, *startCash, index); err != nil {
		fatalf("pnl: %v", err)
	}
}

// --- fills ------------------------------------------------------------------------------------

func readFills(ctx context.Context, api string, from, to time.Time) ([]fill, error) {
	var fills []fill
	before := int64(0)
	for page := 0; page < 200; page++ {
		url := fmt.Sprintf("%s/v1/trades?asset_address=%s&sub_id=0&limit=100", strings.TrimRight(api, "/"), defaultPerp)
		if before > 0 {
			url += "&before_trade_id=" + strconv.FormatInt(before, 10)
		}
		var payload struct {
			Trades []trade `json:"trades"`
		}
		if err := getJSON(ctx, url, &payload); err != nil {
			return nil, err
		}
		if len(payload.Trades) == 0 {
			break
		}
		done := false
		for _, t := range payload.Trades {
			at, err := time.Parse(time.RFC3339Nano, t.CreatedAt)
			if err != nil {
				return nil, fmt.Errorf("trade %d created_at %q: %w", t.TradeID, t.CreatedAt, err)
			}
			if at.Before(from) {
				done = true
				break
			}
			before = t.TradeID
			if at.After(to) {
				continue
			}
			role := ""
			switch {
			case isBotOrderID(t.MakerOrderID):
				role = "maker"
			case isBotOrderID(t.TakerOrderID):
				role = "taker"
			default:
				continue
			}
			takerSide, price, size, err := engineFill(t)
			if err != nil {
				return nil, fmt.Errorf("trade %d: %w", t.TradeID, err)
			}
			// The bot is on the taker's side when it took, and opposite it when it made.
			botSide := botSellsCNGN
			if (takerSide == exchange.SideBuy) == (role == "taker") {
				botSide = botBuysCNGN
			}
			fills = append(fills, fill{at: at, tradeID: t.TradeID, botSide: botSide, size: size, price: price, role: role})
		}
		if done || len(payload.Trades) < 100 {
			break
		}
	}
	sort.Slice(fills, func(i, j int) bool { return fills[i].at.Before(fills[j].at) })
	return fills, nil
}

// engineFill is a trade in engine terms: the taker's side, the price in USDC per cNGN and the
// size in cNGN. The engine fields are preferred; a row carrying only a ui_intent is decoded
// through its own spec, so a tape spanning the venue's cutover reads correctly on both sides.
func engineFill(t trade) (exchange.Side, float64, float64, error) {
	price, perr := strconv.ParseFloat(strings.TrimSpace(t.Price), 64)
	size, serr := strconv.ParseFloat(strings.TrimSpace(t.Size), 64)
	side := exchange.Side(strings.ToLower(strings.TrimSpace(t.AggressorSide)))
	if perr == nil && serr == nil && price > 0 && size > 0 && (side == exchange.SideBuy || side == exchange.SideSell) {
		return side, price, size, nil
	}
	if t.SpotContract == nil {
		return "", 0, 0, fmt.Errorf("no engine fields and no spot_contract")
	}
	intent := t.SpotContract.UIIntent
	return exchange.EngineOrderFromUIIntent(t.SpotContract.Spec, intent.Side, intent.Price, intent.Size)
}

func printFills(fills []fill, indexes []indexPoint) {
	fmt.Printf("Fills: %d\n", len(fills))
	if len(fills) == 0 {
		fmt.Println()
		return
	}
	fmt.Println("  time (UTC)            trade   bot side   size cNGN   price USDC/cNGN  index before  index after   60s adverse move  flag")
	flagged := 0
	for _, f := range fills {
		before, after := indexAround(indexes, f.at)
		adverse, adverseAt := adverseMove(indexes, f, before)
		flag := ""
		if adverse > adverseMoveBPS {
			flag = "ADVERSE"
			flagged++
		}
		moveText := "—"
		if adverseAt != nil {
			moveText = fmt.Sprintf("%+.1f bps at +%ds", adverse, int(adverseAt.Sub(f.at).Seconds()))
		}
		fmt.Printf("  %s  %-6d  %-9s  %9.0f   %15.9f  %12s  %12s  %-18s %s\n",
			f.at.UTC().Format("2006-01-02 15:04:05"), f.tradeID, f.botSide, f.size, f.price,
			fmtIndex(before), fmtIndex(after), moveText, flag)
	}
	fmt.Printf("  flagged: %d fill(s) followed within %s by an index move over %.0f bps against the bot\n\n", flagged, adverseWindow, adverseMoveBPS)
}

// indexAround is the last index published at or before t, and the first after it.
func indexAround(indexes []indexPoint, t time.Time) (before, after *indexPoint) {
	for i := range indexes {
		p := indexes[i]
		if !p.at.After(t) {
			before = &indexes[i]
		} else {
			after = &indexes[i]
			break
		}
	}
	return before, after
}

// adverseMove is the largest move of the index within adverseWindow after the fill, in bps of the
// index before the fill, signed so that positive is against the bot: up (cNGN dearer in USDC) after
// the bot sold cNGN, down after it bought.
func adverseMove(indexes []indexPoint, f fill, before *indexPoint) (float64, *time.Time) {
	if before == nil {
		return 0, nil
	}
	var worst float64
	var worstAt *time.Time
	for i := range indexes {
		p := indexes[i]
		if !p.at.After(f.at) || p.at.After(f.at.Add(adverseWindow)) {
			continue
		}
		move := (p.index - before.index) / before.index * 1e4
		if f.botSide == botBuysCNGN {
			move = -move
		}
		if worstAt == nil || move > worst {
			worst = move
			at := p.at
			worstAt = &at
		}
	}
	return worst, worstAt
}

func fmtIndex(p *indexPoint) string {
	if p == nil {
		return "—"
	}
	return fmt.Sprintf("%.9f", p.index)
}

// --- index history ----------------------------------------------------------------------------

func readIndexHistory(ctx context.Context, client *ethclient.Client, feed common.Address, fromBlock, toBlock uint64) ([]indexPoint, error) {
	topic := crypto.Keccak256Hash([]byte("SpotPriceUpdated(uint96,uint96,uint64)"))
	var points []indexPoint
	err := forEachLog(ctx, client, []common.Address{feed}, [][]common.Hash{{topic}}, fromBlock, toBlock, func(l ethLog) error {
		if len(l.Data) < 96 {
			return nil
		}
		spot := new(big.Int).SetBytes(l.Data[0:32])           // USDC per cNGN, 18dp
		stamp := new(big.Int).SetBytes(l.Data[64:96]).Int64() // the publisher's timestamp
		if spot.Sign() <= 0 {
			return nil
		}
		usdcPerCNGN, _ := new(big.Float).Quo(new(big.Float).SetInt(spot), e18).Float64()
		points = append(points, indexPoint{at: time.Unix(stamp, 0), index: usdcPerCNGN})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(points, func(i, j int) bool { return points[i].at.Before(points[j].at) })
	return points, nil
}

// --- balances ---------------------------------------------------------------------------------

func readBalanceHistory(ctx context.Context, client *ethclient.Client, subAccounts common.Address, account int64, fromBlock, toBlock uint64) ([]balancePoint, error) {
	topic := crypto.Keccak256Hash([]byte("BalanceAdjusted(uint256,address,bytes32,int256,int256,int256,uint256)"))
	accountTopic := common.BigToHash(big.NewInt(account))
	perpTopic := assetTopic(common.HexToAddress(defaultPerp))
	cashTopic := assetTopic(common.HexToAddress(defaultCash))
	points := map[uint64]*balancePoint{}
	err := forEachLog(ctx, client, []common.Address{subAccounts}, [][]common.Hash{{topic}, {accountTopic}, nil, {perpTopic, cashTopic}}, fromBlock, toBlock, func(l ethLog) error {
		if len(l.Topics) < 4 || len(l.Data) < 96 {
			return nil
		}
		post := signedWord(l.Data[64:96])
		value, _ := new(big.Float).Quo(new(big.Float).SetInt(post), e18).Float64()
		p := points[l.BlockNumber]
		if p == nil {
			p = &balancePoint{block: l.BlockNumber}
			points[l.BlockNumber] = p
		}
		if l.Topics[3] == perpTopic {
			p.perp = &value
		} else {
			p.cash = &value
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]balancePoint, 0, len(points))
	for _, p := range points {
		header, err := client.HeaderByNumber(ctx, new(big.Int).SetUint64(p.block))
		if err != nil {
			return nil, err
		}
		p.at = time.Unix(int64(header.Time), 0)
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].block < out[j].block })
	return out, nil
}

func printInventory(points []balancePoint, from time.Time, index float64) {
	fmt.Println("Inventory over time (every on-chain balance change of the account; a fill, funding, fee or settlement)")
	fmt.Println("  time (UTC)            block      position cNGN     position USD*   cash USDC")
	var lastPerp, lastCash *float64
	shown := 0
	for _, p := range points {
		if p.perp != nil {
			lastPerp = p.perp
		}
		if p.cash != nil {
			lastCash = p.cash
		}
		if p.at.Before(from) {
			continue
		}
		shown++
		fmt.Printf("  %s  %-9d  %15s  %14s  %10s\n", p.at.UTC().Format("2006-01-02 15:04:05"), p.block,
			fmtPtr(lastPerp, "%.0f"), fmtUSD(lastPerp, index), fmtPtr(lastCash, "%.4f"))
	}
	if shown == 0 {
		fmt.Println("  (no balance change in the window)")
	}
	fmt.Println("  * USD at the index at report time; a positive cNGN position is long cNGN, negative is short cNGN")
	fmt.Println()
}

// --- P&L --------------------------------------------------------------------------------------

func printPnL(ctx context.Context, client *ethclient.Client, account int64, startCash, index float64) error {
	subAccounts := common.HexToAddress(defaultSubAccounts)
	perp := common.HexToAddress(defaultPerp)
	cash, err := getBalance(ctx, client, subAccounts, account, common.HexToAddress(defaultCash))
	if err != nil {
		return err
	}
	position, err := getBalance(ctx, client, subAccounts, account, perp)
	if err != nil {
		return err
	}
	unrealized, err := callInt(ctx, client, perp, "getUnsettledAndUnrealizedCash(uint256)", common.BigToHash(big.NewInt(account)).Bytes())
	if err != nil {
		return err
	}
	realized := cash - startCash
	fmt.Println("P&L")
	fmt.Printf("  starting cash            %12.4f USDC\n", startCash)
	fmt.Printf("  cash now                 %12.4f USDC\n", cash)
	fmt.Printf("  realized (cash − start)  %+12.4f USDC   fees, funding and settled P&L\n", realized)
	fmt.Printf("  unrealized + unsettled   %+12.4f USDC   PerpAsset.getUnsettledAndUnrealizedCash\n", unrealized)
	fmt.Printf("  total                    %+12.4f USDC\n", realized+unrealized)
	fmt.Printf("  position                 %12.0f cNGN contracts = %+.2f USD at index %.9f USDC/cNGN (%s)\n", position, position*index, index, describePosition(position))
	return nil
}

func describePosition(position float64) string {
	switch {
	case position > 0:
		return "long cNGN"
	case position < 0:
		return "short cNGN"
	}
	return "flat"
}

// --- chain helpers ----------------------------------------------------------------------------

type ethLog struct {
	Topics      []common.Hash
	Data        []byte
	BlockNumber uint64
}

func forEachLog(ctx context.Context, client *ethclient.Client, addresses []common.Address, topics [][]common.Hash, fromBlock, toBlock uint64, fn func(ethLog) error) error {
	for start := fromBlock; start <= toBlock; start += logChunk {
		end := start + logChunk - 1
		if end > toBlock {
			end = toBlock
		}
		logs, err := client.FilterLogs(ctx, ethereum.FilterQuery{
			FromBlock: new(big.Int).SetUint64(start),
			ToBlock:   new(big.Int).SetUint64(end),
			Addresses: addresses,
			Topics:    topics,
		})
		if err != nil {
			return fmt.Errorf("logs %d..%d: %w", start, end, err)
		}
		for _, l := range logs {
			if l.Removed {
				continue
			}
			if err := fn(ethLog{Topics: l.Topics, Data: l.Data, BlockNumber: l.BlockNumber}); err != nil {
				return err
			}
		}
	}
	return nil
}

// blockAtOrAfter finds the first block whose timestamp is at or after t (binary search on headers).
func blockAtOrAfter(ctx context.Context, client *ethclient.Client, t time.Time) (uint64, error) {
	head, err := client.HeaderByNumber(ctx, nil)
	if err != nil {
		return 0, err
	}
	if int64(head.Time) <= t.Unix() {
		return head.Number.Uint64(), nil
	}
	lo, hi := uint64(0), head.Number.Uint64()
	for lo < hi {
		mid := (lo + hi) / 2
		h, err := client.HeaderByNumber(ctx, new(big.Int).SetUint64(mid))
		if err != nil {
			return 0, err
		}
		if int64(h.Time) < t.Unix() {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo, nil
}

func getBalance(ctx context.Context, client *ethclient.Client, subAccounts common.Address, account int64, asset common.Address) (float64, error) {
	args := append(append(common.BigToHash(big.NewInt(account)).Bytes(), common.LeftPadBytes(asset.Bytes(), 32)...), make([]byte, 32)...)
	v, err := callInt(ctx, client, subAccounts, "getBalance(uint256,address,uint256)", args)
	return v, err
}

func callInt(ctx context.Context, client *ethclient.Client, to common.Address, sig string, args []byte) (float64, error) {
	data := append(crypto.Keccak256([]byte(sig))[:4], args...)
	raw, err := client.CallContract(ctx, ethereum.CallMsg{To: &to, Data: data}, nil)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", sig, err)
	}
	if len(raw) < 32 {
		return 0, fmt.Errorf("%s: short return", sig)
	}
	v, _ := new(big.Float).Quo(new(big.Float).SetInt(signedWord(raw[:32])), e18).Float64()
	return v, nil
}

// currentIndex is the feed's spot, USDC per cNGN, used as is.
func currentIndex(ctx context.Context, client *ethclient.Client) (float64, error) {
	feed := common.HexToAddress(defaultIndexFeed)
	usdcPerCNGN, err := callInt(ctx, client, feed, "getSpot()", nil)
	if err != nil {
		return 0, err
	}
	if usdcPerCNGN <= 0 {
		return 0, fmt.Errorf("index feed returned %v", usdcPerCNGN)
	}
	return usdcPerCNGN, nil
}

func assetTopic(asset common.Address) common.Hash {
	// bytes32(uint(uint160(asset)) << 96) | subId 0
	var h common.Hash
	copy(h[:20], asset.Bytes())
	return h
}

func signedWord(word []byte) *big.Int {
	v := new(big.Int).SetBytes(word)
	if len(word) == 32 && word[0]&0x80 != 0 {
		v.Sub(v, new(big.Int).Lsh(big.NewInt(1), 256))
	}
	return v
}

// --- small helpers ----------------------------------------------------------------------------

func parseWindow(fromFlag, toFlag string) (time.Time, time.Time, error) {
	now := time.Now()
	to := now
	if toFlag != "" && toFlag != "now" {
		t, err := time.Parse(time.RFC3339, toFlag)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("-to %q: %w", toFlag, err)
		}
		to = t
	}
	if d, err := time.ParseDuration(fromFlag); err == nil {
		return to.Add(-d), to, nil
	}
	from, err := time.Parse(time.RFC3339, fromFlag)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("-from %q: RFC3339 or a duration like 24h", fromFlag)
	}
	if !from.Before(to) {
		return time.Time{}, time.Time{}, fmt.Errorf("-from must be before -to")
	}
	return from, to, nil
}

func getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func fmtPtr(v *float64, layout string) string {
	if v == nil {
		return "—"
	}
	return fmt.Sprintf(layout, *v)
}

// fmtUSD is the position's USD value at the index (USDC per cNGN): long cNGN positive.
func fmtUSD(perp *float64, index float64) string {
	if perp == nil || index <= 0 {
		return "—"
	}
	return fmt.Sprintf("%+.2f", *perp*index)
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "perp-fill-report: "+format+"\n", args...)
	os.Exit(1)
}

// isBotOrderID recognises the perp market maker's own orders under either name the venue has listed
// the perp under: the bot tags an order with the market name it resolved at the time.
func isBotOrderID(orderID string) bool {
	for _, name := range []string{marketnames.PerpCanonical, marketnames.PerpLegacy} {
		if strings.HasPrefix(orderID, "mm:"+name+":") {
			return true
		}
	}
	return false
}
