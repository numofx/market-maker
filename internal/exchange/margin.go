package exchange

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

// CancelReasonInsufficientMargin is the venue's reason for cancelling a futures order whose account
// could not carry its fill: markets-service holds it back, and after three refusals cancels it.
const CancelReasonInsufficientMargin = "insufficient_margin"

// CancelReasons reads, for orders this bot's subaccount placed, why each one left the book: the
// venue's cancel_reason for a cancelled order, "" for one that filled or expired. Orders not found are
// absent from the result.
func (c *HTTPClient) CancelReasons(ctx context.Context, orderIDs []string) (map[string]string, error) {
	out := make(map[string]string, len(orderIDs))
	if len(orderIDs) == 0 {
		return out, nil
	}
	rows, err := c.pg.Query(ctx, `
select order_id, case when status = 'cancelled' then coalesce(cancel_reason, '') else '' end
from active_orders
where order_id = any($1) and subaccount_id = $2`, orderIDs, c.cfg.SubaccountID)
	if err != nil {
		return nil, fmt.Errorf("query cancel reasons: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, reason string
		if err := rows.Scan(&id, &reason); err != nil {
			return nil, err
		}
		out[id] = reason
	}
	return out, rows.Err()
}

var marginABI = mustABI(`[
{"name":"getMargin","type":"function","stateMutability":"view","inputs":[{"name":"accountId","type":"uint256"},{"name":"isInitial","type":"bool"}],"outputs":[{"name":"margin","type":"int256"}]},
{"name":"assetDetails","type":"function","stateMutability":"view","inputs":[{"name":"asset","type":"address"}],"outputs":[{"name":"","type":"tuple","components":[{"name":"isWhitelisted","type":"bool"},{"name":"assetType","type":"uint8"},{"name":"marketId","type":"uint256"}]}]},
{"name":"futureMarginRequirements","type":"function","stateMutability":"view","inputs":[{"name":"marketId","type":"uint256"}],"outputs":[{"name":"mmReq","type":"uint256"},{"name":"imReq","type":"uint256"}]},
{"name":"indexFeed","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"address"}]},
{"name":"getSpot","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"spot","type":"uint256"},{"name":"confidence","type":"uint256"}]},
{"name":"markIndexBand","type":"function","stateMutability":"view","inputs":[{"name":"expiry","type":"uint64"}],"outputs":[{"name":"","type":"uint256"}]},
{"name":"fixingWindow","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"uint256"}]},
{"name":"futureContracts","type":"function","stateMutability":"view","inputs":[{"name":"subId","type":"uint256"}],"outputs":[{"name":"","type":"tuple","components":[{"name":"listed","type":"bool"},{"name":"settled","type":"bool"},{"name":"expiry","type":"uint64"},{"name":"lastTradeTime","type":"uint64"},{"name":"lastMarkTime","type":"uint64"},{"name":"markPrice","type":"uint256"},{"name":"cumulativeVMPerContract","type":"int256"}]}]}
]`)

func mustABI(definition string) abi.ABI {
	parsed, err := abi.JSON(strings.NewReader(definition))
	if err != nil {
		panic(err)
	}
	return parsed
}

// FutureIMHeadroom reads the subaccount's initial-margin surplus on the market's manager (the
// MarginEngine) and the IM one contract adds there, both in USD: getMargin(account, true), and
// contractSize x futureMarginRequirements(marketId).imReq. The engine refuses a fill that leaves the
// surplus below zero, so an order of n contracts needs a surplus of at least n x perContract.
func (c *HTTPClient) FutureIMHeadroom(ctx context.Context, spec MarketSpec) (surplus float64, perContract float64, err error) {
	if spec.MarginManager == "" || spec.ContractSizeUSDC <= 0 {
		return 0, 0, errors.New("the market lists no margin manager or contract size")
	}
	account, ok := new(big.Int).SetString(c.cfg.SubaccountID, 10)
	if !ok {
		return 0, 0, fmt.Errorf("invalid subaccount id %q", c.cfg.SubaccountID)
	}
	engine := common.HexToAddress(spec.MarginManager)

	margin, err := c.callMargin(ctx, engine, "getMargin", account, true)
	if err != nil {
		return 0, 0, err
	}
	details, err := c.callMargin(ctx, engine, "assetDetails", common.HexToAddress(spec.AssetAddress))
	if err != nil {
		return 0, 0, err
	}
	detail := *abi.ConvertType(details[0], new(struct {
		IsWhitelisted bool
		AssetType     uint8
		MarketId      *big.Int
	})).(*struct {
		IsWhitelisted bool
		AssetType     uint8
		MarketId      *big.Int
	})
	if !detail.IsWhitelisted || detail.MarketId == nil {
		return 0, 0, errors.New("the future is not whitelisted on the margin manager")
	}
	reqs, err := c.callMargin(ctx, engine, "futureMarginRequirements", detail.MarketId)
	if err != nil {
		return 0, 0, err
	}
	imReq, ok := reqs[1].(*big.Int)
	if !ok || imReq.Sign() == 0 {
		return 0, 0, errors.New("the manager has no futures IM requirement")
	}
	marginWei, ok := margin[0].(*big.Int)
	if !ok {
		return 0, 0, fmt.Errorf("unexpected getMargin payload %T", margin[0])
	}
	return rawBigToFloat(marginWei), spec.ContractSizeUSDC * rawBigToFloat(imReq), nil
}

func (c *HTTPClient) callMargin(ctx context.Context, to common.Address, method string, args ...any) ([]any, error) {
	input, err := marginABI.Pack(method, args...)
	if err != nil {
		return nil, err
	}
	output, err := c.rpc.CallContract(ctx, ethereumCallMsg(to, input), nil)
	if err != nil {
		return nil, fmt.Errorf("%s rpc: %w", method, err)
	}
	values, err := marginABI.Unpack(method, output)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", method, err)
	}
	return values, nil
}

// IsMarginEngineFuture reports whether the market is the cNGN future under the MarginEngine: a future
// whose listing carries its contract size (the venue's `future` block). The DFXM-era dated futures this
// bot was built for carry none.
func (s MarketSpec) IsMarginEngineFuture() bool {
	return s.Kind == MarketKindFuture && s.ContractSizeUSDC > 0
}

// FutureState is the MarginEngine future as the bot sizes, prices and times its quotes on it, read from
// the chain every cycle. Prices are cNGN per USDC; margin is USD.
type FutureState struct {
	// IMSurplus is getMargin(account, true); PerContractIM the IM one more contract adds.
	IMSurplus     float64
	PerContractIM float64
	// Index is 1e36 / indexFeed.getSpot(), and the band FutureAsset refuses fills outside of is
	// Index -/+ BandWidth (markIndexBand(expiry)): BandLow to BandHigh.
	Index     float64
	BandWidth float64
	BandLow   float64
	BandHigh  float64
	// LastTradeTime is the series' own; FixingStart is expiry - fixingWindow, when the settlement
	// fixing begins. Quoting stops before the earlier of the two.
	LastTradeTime time.Time
	FixingStart   time.Time
	Settled       bool
}

// QuotingCutoff is when quoting stops: the earlier of the fixing window's start and last trade, less
// the configured margin.
func (f FutureState) QuotingCutoff(before time.Duration) time.Time {
	end := f.FixingStart
	if f.LastTradeTime.Before(end) {
		end = f.LastTradeTime
	}
	return end.Add(-before)
}

// FutureState reads the future's margin, band and schedule. The schedule (index feed, fixing window,
// last trade time) cannot change for a listed series and is read once.
func (c *HTTPClient) FutureState(ctx context.Context, spec MarketSpec) (FutureState, error) {
	if !spec.IsMarginEngineFuture() {
		return FutureState{}, errors.New("not the MarginEngine future")
	}
	surplus, perContract, err := c.FutureIMHeadroom(ctx, spec)
	if err != nil {
		return FutureState{}, err
	}
	future := common.HexToAddress(spec.AssetAddress)
	subID, ok := new(big.Int).SetString(spec.SubID, 10)
	if !ok {
		return FutureState{}, fmt.Errorf("invalid future sub id %q", spec.SubID)
	}

	c.futureMu.Lock()
	schedule := c.futureSchedule
	c.futureMu.Unlock()
	if schedule == nil || schedule.asset != future || schedule.subID.Cmp(subID) != 0 {
		feedOut, err := c.callMargin(ctx, future, "indexFeed")
		if err != nil {
			return FutureState{}, err
		}
		windowOut, err := c.callMargin(ctx, future, "fixingWindow")
		if err != nil {
			return FutureState{}, err
		}
		contractOut, err := c.callMargin(ctx, future, "futureContracts", subID)
		if err != nil {
			return FutureState{}, err
		}
		contract := *abi.ConvertType(contractOut[0], new(futureContract)).(*futureContract)
		if !contract.Listed || contract.Expiry == 0 {
			return FutureState{}, fmt.Errorf("series %s is not listed on %s", spec.SubID, spec.AssetAddress)
		}
		window, _ := windowOut[0].(*big.Int)
		feed, _ := feedOut[0].(common.Address)
		if window == nil || feed == (common.Address{}) {
			return FutureState{}, errors.New("the future has no index feed or fixing window")
		}
		schedule = &futureSchedule{asset: future, subID: subID, feed: feed, expiry: contract.Expiry,
			lastTrade:   time.Unix(int64(contract.LastTradeTime), 0).UTC(),
			fixingStart: time.Unix(int64(contract.Expiry)-window.Int64(), 0).UTC()}
		c.futureMu.Lock()
		c.futureSchedule = schedule
		c.futureMu.Unlock()
	}

	spotOut, err := c.callMargin(ctx, schedule.feed, "getSpot")
	if err != nil {
		return FutureState{}, fmt.Errorf("index unreadable (a stale feed reverts BLF_DataTooOld): %w", err)
	}
	spot, _ := spotOut[0].(*big.Int)
	if spot == nil || spot.Sign() == 0 {
		return FutureState{}, errors.New("index feed returned zero")
	}
	bandOut, err := c.callMargin(ctx, future, "markIndexBand", schedule.expiry)
	if err != nil {
		return FutureState{}, err
	}
	width, _ := bandOut[0].(*big.Int)
	contractOut, err := c.callMargin(ctx, future, "futureContracts", subID)
	if err != nil {
		return FutureState{}, err
	}
	contract := *abi.ConvertType(contractOut[0], new(futureContract)).(*futureContract)

	// index = 1e36 / spot, in 18dp: FutureAsset._indexPrice
	index := new(big.Int).Quo(new(big.Int).Mul(decimalScale, new(big.Int).Mul(decimalScale, decimalScale)), new(big.Int).Mul(spot, decimalScale))
	state := FutureState{
		IMSurplus: surplus, PerContractIM: perContract,
		Index: rawBigToFloat(index), BandWidth: rawBigToFloat(width),
		LastTradeTime: schedule.lastTrade, FixingStart: schedule.fixingStart, Settled: contract.Settled,
	}
	state.BandLow = state.Index * (1 - state.BandWidth)
	state.BandHigh = state.Index * (1 + state.BandWidth)
	return state, nil
}

type futureContract struct {
	Listed                  bool
	Settled                 bool
	Expiry                  uint64
	LastTradeTime           uint64
	LastMarkTime            uint64
	MarkPrice               *big.Int
	CumulativeVMPerContract *big.Int
}

type futureSchedule struct {
	asset       common.Address
	subID       *big.Int
	feed        common.Address
	expiry      uint64
	lastTrade   time.Time
	fixingStart time.Time
}

// futureOrderExpiryCap is the latest an order on the future may be signed to expire: the quoting
// cutoff, once the schedule is known. Zero until FutureState has read it.
func (c *HTTPClient) futureOrderExpiryCap() time.Time {
	c.futureMu.Lock()
	defer c.futureMu.Unlock()
	if c.futureSchedule == nil {
		return time.Time{}
	}
	end := c.futureSchedule.fixingStart
	if c.futureSchedule.lastTrade.Before(end) {
		end = c.futureSchedule.lastTrade
	}
	return end.Add(-time.Duration(c.cfg.FutureCutoffBeforeFixSeconds) * time.Second)
}

// cappedOrderExpiry is an order's signed expiry on this market. On the MarginEngine future no order may
// outlive the quoting cutoff: one signed before it would otherwise rest, and fill, into the fixing window
// or past last trade. A cutoff not yet read refuses the order rather than signing it uncapped.
func cappedOrderExpiry(spec MarketSpec, expiry int64, cutoff time.Time, now time.Time) (int64, error) {
	if !spec.IsMarginEngineFuture() {
		return expiry, nil
	}
	if cutoff.IsZero() {
		return 0, errors.New("the future's schedule has not been read; refusing to sign an order without its expiry cap")
	}
	if expiry > cutoff.Unix() {
		expiry = cutoff.Unix()
	}
	if expiry <= now.Unix() {
		return 0, fmt.Errorf("past the future's quoting cutoff (%s)", cutoff.Format(time.RFC3339))
	}
	return expiry, nil
}
