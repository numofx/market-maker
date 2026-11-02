package exchange

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// cNGN options on MarginEngine v2 (exchange risk-core deploy-cngn-options-v2.s.sol). One OptionAsset per expiry; each
// strike and side is its own market on /v1/markets (contract_type "option", with an `option` block).

// OptionTerms is an option market's contract: strike, side, expiry and where its vol comes from.
type OptionTerms struct {
	Strike        float64 // USDC per base
	StrikeWei     *big.Int
	Call          bool
	Expiry        int64
	LastTradeTime int64
	VolFeed       string
	// UnitBase is the base units one desired_amount is (amount_scale / 1e18): 1,000 cNGN.
	UnitBase float64
}

var optionABI = mustABI(`[
{"name":"settlementFeed","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"address"}]},
{"name":"future","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"address"}]},
{"name":"forwardFeed","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"address"}]},
{"name":"getForwardPrice","type":"function","stateMutability":"view","inputs":[{"name":"expiry","type":"uint64"}],"outputs":[{"name":"","type":"uint256"},{"name":"","type":"uint256"}]},
{"name":"getVol","type":"function","stateMutability":"view","inputs":[{"name":"strike","type":"uint128"},{"name":"expiry","type":"uint64"}],"outputs":[{"name":"vol","type":"uint256"},{"name":"confidence","type":"uint256"}]},
{"name":"getExpiryMinConfidence","type":"function","stateMutability":"view","inputs":[{"name":"expiry","type":"uint64"}],"outputs":[{"name":"","type":"uint256"}]},
{"name":"assetDetails","type":"function","stateMutability":"view","inputs":[{"name":"asset","type":"address"}],"outputs":[{"name":"","type":"tuple","components":[{"name":"isWhitelisted","type":"bool"},{"name":"assetType","type":"uint8"},{"name":"marketId","type":"uint256"}]}]},
{"name":"getMargin","type":"function","stateMutability":"view","inputs":[{"name":"accountId","type":"uint256"},{"name":"isInitial","type":"bool"}],"outputs":[{"name":"margin","type":"int256"}]},
{"name":"getIsolatedMargin","type":"function","stateMutability":"view","inputs":[{"name":"marketId","type":"uint256"},{"name":"strike","type":"uint256"},{"name":"expiry","type":"uint256"},{"name":"isCall","type":"bool"},{"name":"balance","type":"int256"},{"name":"isInitial","type":"bool"}],"outputs":[{"name":"margin","type":"int256"},{"name":"markToMarket","type":"int256"}]}
]`)

// assetTypeOption is IMarginEngine.AssetType.Option.
const assetTypeOption = 1

// ErrVolUnavailable is a vol feed that reverts for the expiry: no surface, or older than its heartbeat. The engine cannot
// margin an option then, so nothing should be quoted.
var ErrVolUnavailable = errors.New("the vol feed has no live surface for the expiry")

// OptionInputs are what the engine marks a strike with, read at one block: Black-76 on these gives the engine's mark.
type OptionInputs struct {
	Block         uint64
	BlockTime     int64   // the block's timestamp: the engine's "now"
	Forward       float64 // USDC per base at the expiry
	Vol           float64 // annualised, at this strike
	VolConfidence float64 // 1.0 normally; under the engine's option threshold the vol publisher has degraded
}

// optionChain caches, per OptionAsset, the immutable links from it to its forward feed and its engine market id.
type optionChain struct {
	mu       sync.Mutex
	forward  map[common.Address]common.Address // option -> its future's forward feed
	marketID map[common.Address]*big.Int       // option -> engine market id
}

var options = optionChain{forward: map[common.Address]common.Address{}, marketID: map[common.Address]*big.Int{}}

// OptionInputs reads the forward (option -> settlement adapter -> dated future -> forward feed, the chain the engine
// prices with) and the strike's vol at `block` (nil: latest). ErrVolUnavailable when the vol feed reverts.
func (c *HTTPClient) OptionInputs(ctx context.Context, spec MarketSpec, block *big.Int) (OptionInputs, error) {
	if spec.Option == nil {
		return OptionInputs{}, fmt.Errorf("%s is not an option market", spec.Symbol)
	}
	return readOptionInputs(ctx, c.rpc, common.HexToAddress(spec.AssetAddress), *spec.Option, block)
}

// contractCaller is what the option reads need of an ethclient: calls and headers at a block.
type contractCaller interface {
	CallContract(ctx context.Context, msg ethereum.CallMsg, block *big.Int) ([]byte, error)
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
}

func readOptionInputs(ctx context.Context, rpc contractCaller, option common.Address, terms OptionTerms, block *big.Int) (OptionInputs, error) {
	header, err := rpc.HeaderByNumber(ctx, block)
	if err != nil {
		return OptionInputs{}, fmt.Errorf("header: %w", err)
	}
	at := header.Number
	forwardFeed, err := forwardFeedOf(ctx, rpc, option, at)
	if err != nil {
		return OptionInputs{}, err
	}
	expiry := uint64(terms.Expiry)
	fwd, err := callAt(ctx, rpc, forwardFeed, at, "getForwardPrice", expiry)
	if err != nil {
		return OptionInputs{}, err
	}
	vol, err := callAt(ctx, rpc, common.HexToAddress(terms.VolFeed), at, "getVol", terms.StrikeWei, expiry)
	if err != nil {
		var reverted interface{ ErrorData() any }
		if errors.As(err, &reverted) || strings.Contains(strings.ToLower(err.Error()), "revert") {
			return OptionInputs{}, ErrVolUnavailable
		}
		return OptionInputs{}, err
	}
	return OptionInputs{
		Block:         at.Uint64(),
		BlockTime:     int64(header.Time),
		Forward:       wad(fwd[0].(*big.Int)),
		Vol:           wad(vol[0].(*big.Int)),
		VolConfidence: wad(vol[1].(*big.Int)),
	}, nil
}

// EngineOptionMark is the engine's own isolated margin and mark-to-market for `balance` (18dp, signed) of the strike,
// at `block`: what a quote is checked against, and a writer's requirement (margin, negative).
func (c *HTTPClient) EngineOptionMark(ctx context.Context, spec MarketSpec, balance *big.Int, initial bool, block *big.Int) (margin, mark float64, err error) {
	if spec.Option == nil {
		return 0, 0, fmt.Errorf("%s is not an option market", spec.Symbol)
	}
	return readEngineOptionMark(ctx, c.rpc, common.HexToAddress(spec.MarginManager), common.HexToAddress(spec.AssetAddress), *spec.Option, balance, initial, block)
}

func readEngineOptionMark(ctx context.Context, rpc contractCaller, engine, option common.Address, terms OptionTerms, balance *big.Int, initial bool, block *big.Int) (float64, float64, error) {
	market, err := optionMarketID(ctx, rpc, engine, option, block)
	if err != nil {
		return 0, 0, err
	}
	out, err := callAt(ctx, rpc, engine, block, "getIsolatedMargin", market, terms.StrikeWei, big.NewInt(terms.Expiry), terms.Call, balance, initial)
	if err != nil {
		return 0, 0, err
	}
	return wad(out[0].(*big.Int)), wad(out[1].(*big.Int)), nil
}

// optionMarketID reads assetDetails(option): struct AssetDetail { bool isWhitelisted; AssetType assetType; uint marketId; }.
// The type must be Option, and the market id is the third field (the second is the type).
func optionMarketID(ctx context.Context, rpc contractCaller, engine, option common.Address, block *big.Int) (*big.Int, error) {
	options.mu.Lock()
	if id, ok := options.marketID[option]; ok {
		options.mu.Unlock()
		return id, nil
	}
	options.mu.Unlock()
	out, err := callAt(ctx, rpc, engine, block, "assetDetails", option)
	if err != nil {
		return nil, err
	}
	detail := out[0].(struct {
		IsWhitelisted bool     `json:"isWhitelisted"`
		AssetType     uint8    `json:"assetType"`
		MarketId      *big.Int `json:"marketId"`
	})
	if !detail.IsWhitelisted {
		return nil, errors.New("the engine does not whitelist the option")
	}
	if detail.AssetType != assetTypeOption {
		return nil, fmt.Errorf("the engine lists the asset as type %d, not an option", detail.AssetType)
	}
	options.mu.Lock()
	options.marketID[option] = detail.MarketId
	options.mu.Unlock()
	return detail.MarketId, nil
}

func forwardFeedOf(ctx context.Context, rpc contractCaller, option common.Address, block *big.Int) (common.Address, error) {
	options.mu.Lock()
	if feed, ok := options.forward[option]; ok {
		options.mu.Unlock()
		return feed, nil
	}
	options.mu.Unlock()
	link := option
	for _, method := range []string{"settlementFeed", "future", "forwardFeed"} {
		out, err := callAt(ctx, rpc, link, block, method)
		if err != nil {
			return common.Address{}, err
		}
		link = out[0].(common.Address)
		if link == (common.Address{}) {
			return common.Address{}, fmt.Errorf("%s is unset on the way to the option's forward feed", method)
		}
	}
	options.mu.Lock()
	options.forward[option] = link
	options.mu.Unlock()
	return link, nil
}

func callAt(ctx context.Context, rpc contractCaller, to common.Address, block *big.Int, method string, args ...any) ([]any, error) {
	input, err := optionABI.Pack(method, args...)
	if err != nil {
		return nil, err
	}
	output, err := rpc.CallContract(ctx, ethereumCallMsg(to, input), block)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	values, err := optionABI.Unpack(method, output)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", method, err)
	}
	return values, nil
}

// wad is an 18dp fixed-point value as a float64, signed.
func wad(v *big.Int) float64 {
	f, _ := new(big.Float).Quo(new(big.Float).SetInt(v), big.NewFloat(1e18)).Float64()
	return f
}

// strikeWei is a decimal strike in 18dp on OptionEncoding's 1e-8 grid, as the venue lists it.
func strikeWei(strike string) (*big.Int, float64, error) {
	rat, ok := new(big.Rat).SetString(strings.TrimSpace(strike))
	if !ok || rat.Sign() <= 0 {
		return nil, 0, fmt.Errorf("strike %q is not a positive number", strike)
	}
	w := new(big.Rat).Mul(rat, new(big.Rat).SetInt(decimalScale))
	if !w.IsInt() || new(big.Int).Mod(w.Num(), big.NewInt(1e10)).Sign() != 0 {
		return nil, 0, fmt.Errorf("strike %q is off the 1e-8 grid", strike)
	}
	f, _ := rat.Float64()
	if math.IsNaN(f) || f <= 0 {
		return nil, 0, fmt.Errorf("strike %q", strike)
	}
	return w.Num(), f, nil
}

// abiUnused keeps the abi import honest if the ABI moves.
var _ = abi.ABI{}

// OptionState is one cycle's chain view of an option strike for the quoter, all read at one block.
type OptionState struct {
	OptionInputs
	// VolUnavailable: the vol feed reverted for the expiry (no surface, or past its heartbeat). The engine cannot margin
	// a writer then; nothing is quoted.
	VolUnavailable bool
	// IMSurplus is getMargin(account, true) on the engine, USDC: what a fill may consume.
	IMSurplus float64
	// WriteIMPerBase is the initial margin one more written base unit (cNGN) needs, standalone, USDC: -margin of
	// getIsolatedMargin(-1 unit, initial) / unit. Conservative for an account already holding offsetting longs.
	WriteIMPerBase float64
}

// OptionState reads the strike's forward, vol, the account's IM surplus and a writer's per-unit requirement at one block.
func (c *HTTPClient) OptionState(ctx context.Context, spec MarketSpec) (OptionState, error) {
	if spec.Option == nil || !(spec.Option.UnitBase > 0) {
		return OptionState{}, fmt.Errorf("%s is not a priced option market", spec.Symbol)
	}
	account, ok := new(big.Int).SetString(c.cfg.SubaccountID, 10)
	if !ok {
		return OptionState{}, fmt.Errorf("invalid subaccount id %q", c.cfg.SubaccountID)
	}
	return readOptionState(ctx, c.rpc, common.HexToAddress(spec.MarginManager), common.HexToAddress(spec.AssetAddress), *spec.Option, spec.AmountScale, account)
}

func readOptionState(ctx context.Context, rpc contractCaller, engine, option common.Address, terms OptionTerms, amountScale string, account *big.Int) (OptionState, error) {
	in, err := readOptionInputs(ctx, rpc, option, terms, nil)
	if errors.Is(err, ErrVolUnavailable) {
		return OptionState{VolUnavailable: true}, nil
	}
	if err != nil {
		return OptionState{}, err
	}
	at := new(big.Int).SetUint64(in.Block)
	surplus, err := callAt(ctx, rpc, engine, at, "getMargin", account, true)
	if err != nil {
		return OptionState{}, err
	}
	unit, ok := new(big.Int).SetString(strings.TrimSpace(amountScale), 10)
	if !ok || unit.Sign() <= 0 {
		return OptionState{}, fmt.Errorf("amount_scale %q", amountScale)
	}
	margin, _, err := readEngineOptionMark(ctx, rpc, engine, option, terms, new(big.Int).Neg(unit), true, at)
	if err != nil {
		return OptionState{}, err
	}
	return OptionState{OptionInputs: in, IMSurplus: wad(surplus[0].(*big.Int)), WriteIMPerBase: -margin / terms.UnitBase}, nil
}

// OptionMarkets are the listed, priced option strikes of `series` (e.g. "cNGN-30NOV26"), by symbol.
func (c *HTTPClient) OptionMarkets(series string) []MarketSpec {
	prefix := strings.TrimSpace(series) + "-"
	var out []MarketSpec
	for _, spec := range c.marketsSnapshot() {
		if spec.IsOption() && spec.Option != nil && spec.Option.UnitBase > 0 && strings.HasPrefix(spec.Symbol, prefix) {
			out = append(out, spec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out
}

// CheckOptionWiring refuses to quote an option the bot would sign for wrongly: the configured TradeModule and its
// quoteAsset must be the listing's, and MM_SUBACCOUNT_ID must be margined by the option's engine.
func (c *HTTPClient) CheckOptionWiring(ctx context.Context, spec MarketSpec) error {
	if !spec.IsOption() {
		return nil
	}
	if err := optionWiringError(spec, c.tradeModule.Hex(), c.quoteAsset.Hex()); err != nil {
		return err
	}
	manager, err := c.subaccountManager(ctx)
	if err != nil {
		return err
	}
	if !strings.EqualFold(manager.Hex(), spec.MarginManager) {
		return fmt.Errorf("MM_SUBACCOUNT_ID %s is under manager %s, not %s's engine %s", c.cfg.SubaccountID, manager.Hex(), spec.Symbol, spec.MarginManager)
	}
	return nil
}

func optionWiringError(spec MarketSpec, tradeModule, quoteAsset string) error {
	switch {
	case spec.Option == nil || !(spec.Option.UnitBase > 0) || spec.Option.VolFeed == "":
		return fmt.Errorf("%s lists no option terms, unit or vol feed", spec.Symbol)
	case spec.MarginManager == "":
		return fmt.Errorf("%s lists no margin manager", spec.Symbol)
	case !strings.EqualFold(tradeModule, spec.ListedTradeModule):
		return fmt.Errorf("MM_TRADE_MODULE_ADDRESS %s is not %s's trade module %s", tradeModule, spec.Symbol, spec.ListedTradeModule)
	case !strings.EqualFold(quoteAsset, spec.ListedQuoteAsset):
		return fmt.Errorf("the module's quoteAsset %s is not %s's quote asset %s", quoteAsset, spec.Symbol, spec.ListedQuoteAsset)
	}
	return nil
}
