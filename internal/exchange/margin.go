package exchange

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

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
{"name":"futureMarginRequirements","type":"function","stateMutability":"view","inputs":[{"name":"marketId","type":"uint256"}],"outputs":[{"name":"mmReq","type":"uint256"},{"name":"imReq","type":"uint256"}]}
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

// ErrFuturesModelNotRebuilt refuses to quote the MarginEngine future. The bot's futures model is the
// DFXM-era one: its notional is contracts x price, where the future's is contracts x contract size (1,000
// USDC), so MM_MAX_NOTIONAL_PER_SIDE and the per-side budget mean something else entirely; and nothing
// stops it quoting into expiry, through the fixing window and past last trade. Rebuilding both is a
// launch requirement of the future (numofx/exchange margin-engine-review.md, enable requirements); until
// it lands, this is the guard.
var ErrFuturesModelNotRebuilt = errors.New("the cNGN future under the MarginEngine needs the rebuilt futures model " +
	"(notional = contracts x contract size, no quoting near expiry) before this bot may quote it")
