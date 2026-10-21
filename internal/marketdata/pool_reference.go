package marketdata

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// ErrAnchorOutOfBand marks a live reference the oracle says is wrong: the pool's TWAP is further from
// the Chainlink price than MM_ANCHOR_BAND_BPS. The bot halts on it at once, as on a stale oracle.
var ErrAnchorOutOfBand = errors.New("anchor out of band")

// baseUSDC is Circle USDC on Base: the pool must pair the market's base with it, and the price is read
// as USDC per base whichever side of the pool it sits on.
var baseUSDC = common.HexToAddress("0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913")

// poolRefreshThrottle bounds how often the pool is re-read; the quote loop is served the last value
// in between. A TWAP moves slowly by construction, so this costs nothing in freshness.
const poolRefreshThrottle = 5 * time.Second

// PoolBandedAnchorSource is the chainlink anchor with a live reference: MM_ANCHOR_SOURCE_TYPE=chainlink
// plus MM_ANCHOR_POOL_ADDRESS. The bot quotes around a Uniswap-v3-style pool's time-weighted price
// (observe() over MM_ANCHOR_POOL_TWAP_SECONDS), which moves with the market every block, instead of the
// Chainlink answer, which moves only past its deviation threshold (0.3% on the 24h EURC/USD feed, 0.1%
// on the 1h one) and so lags the market by up to that much.
//
// Chainlink stays the sanity band. Every read takes the oracle first, with all of its refusals (a stale
// or non-positive round halts at once), then refuses a TWAP more than bandBps from it with
// ErrAnchorOutOfBand. So the most a pool can be pushed is the band, and holding it there for the TWAP
// window costs the pusher the pool's depth; past the band the bot stops quoting instead of following.
type PoolBandedAnchorSource struct {
	oracle  AnchorSource
	rpcURL  string
	pool    common.Address
	twap    time.Duration
	bandBps float64
	client  *http.Client
	now     func() time.Time
	refresh time.Duration

	mu     sync.Mutex
	layout *poolLayout
	price  float64
	readAt time.Time
}

// poolLayout is read once: which side the base is on, and the decimals that scale the price.
type poolLayout struct {
	baseIsToken0 bool
	scale        float64 // 10^(decimals0 - decimals1)
}

func NewPoolBandedAnchorSource(oracle AnchorSource, rpcURL, pool string, twap time.Duration, bandBps float64) *PoolBandedAnchorSource {
	return &PoolBandedAnchorSource{
		oracle:  oracle,
		rpcURL:  rpcURL,
		pool:    common.HexToAddress(pool),
		twap:    twap,
		bandBps: bandBps,
		client:  &http.Client{Timeout: 5 * time.Second},
		now:     time.Now,
		refresh: poolRefreshThrottle,
	}
}

// Name stays the oracle's: the anchor is still Chainlink-guarded, and the strategy keys "quote around
// the anchor, book or no book" on it.
func (s *PoolBandedAnchorSource) Name() string { return s.oracle.Name() }

func (s *PoolBandedAnchorSource) GetAnchorPrice(ctx context.Context, market string) (float64, error) {
	oracle, err := s.oracle.GetAnchorPrice(ctx, market)
	if err != nil {
		return 0, err
	}
	reference, err := s.poolPrice(ctx)
	if err != nil {
		return 0, err
	}
	if deviation := math.Abs(reference-oracle) / oracle * 10_000; deviation > s.bandBps {
		return 0, fmt.Errorf("%w: pool %s %.0fs TWAP %.6f is %.1f bps from Chainlink %.6f (band %.0f bps)",
			ErrAnchorOutOfBand, s.pool.Hex(), s.twap.Seconds(), reference, deviation, oracle, s.bandBps)
	}
	return reference, nil
}

func (s *PoolBandedAnchorSource) poolPrice(ctx context.Context) (float64, error) {
	now := s.now()
	s.mu.Lock()
	if s.price > 0 && s.refresh > 0 && now.Sub(s.readAt) < s.refresh {
		price := s.price
		s.mu.Unlock()
		return price, nil
	}
	s.mu.Unlock()

	layout, err := s.readLayout(ctx)
	if err != nil {
		return 0, err
	}
	tick, err := s.twapTick(ctx)
	if err != nil {
		return 0, err
	}
	price := poolTickPrice(tick, *layout)
	if !(price > 0) || math.IsInf(price, 0) {
		return 0, fmt.Errorf("%w: pool %s TWAP tick %d gives price %v", ErrAnchorInvalid, s.pool.Hex(), tick, price)
	}
	s.mu.Lock()
	s.price, s.readAt = price, now
	s.mu.Unlock()
	return price, nil
}

// poolTickPrice is USDC per base at a tick: 1.0001^tick is token1 per token0 in raw units.
func poolTickPrice(tick int64, layout poolLayout) float64 {
	raw := math.Pow(1.0001, float64(tick)) * layout.scale
	if layout.baseIsToken0 {
		return raw
	}
	return 1 / raw
}

// twapTick is the arithmetic-mean tick over the window, rounded toward negative infinity as the
// Uniswap v3 OracleLibrary does.
func (s *PoolBandedAnchorSource) twapTick(ctx context.Context) (int64, error) {
	seconds := uint32(s.twap / time.Second)
	input, err := poolABI.Pack("observe", []uint32{seconds, 0})
	if err != nil {
		return 0, err
	}
	raw, err := ethCall(ctx, s.client, s.rpcURL, s.pool, input)
	if err != nil {
		return 0, fmt.Errorf("pool %s observe([%d,0]): %w", s.pool.Hex(), seconds, err)
	}
	values, err := poolABI.Unpack("observe", raw)
	if err != nil || len(values) != 2 {
		return 0, fmt.Errorf("pool %s observe: decode: %v", s.pool.Hex(), err)
	}
	cumulatives, ok := values[0].([]*big.Int)
	if !ok || len(cumulatives) != 2 {
		return 0, fmt.Errorf("pool %s observe: unexpected tickCumulatives %T", s.pool.Hex(), values[0])
	}
	return meanTick(cumulatives[0], cumulatives[1], int64(seconds)), nil
}

func meanTick(then, now *big.Int, seconds int64) int64 {
	delta := new(big.Int).Sub(now, then).Int64()
	tick := delta / seconds
	if delta < 0 && delta%seconds != 0 {
		tick--
	}
	return tick
}

func (s *PoolBandedAnchorSource) readLayout(ctx context.Context) (*poolLayout, error) {
	s.mu.Lock()
	if s.layout != nil {
		layout := s.layout
		s.mu.Unlock()
		return layout, nil
	}
	s.mu.Unlock()
	token0, err := s.address(ctx, s.pool, "token0")
	if err != nil {
		return nil, err
	}
	token1, err := s.address(ctx, s.pool, "token1")
	if err != nil {
		return nil, err
	}
	if token0 != baseUSDC && token1 != baseUSDC {
		return nil, fmt.Errorf("pool %s pairs %s and %s: neither is USDC %s", s.pool.Hex(), token0.Hex(), token1.Hex(), baseUSDC.Hex())
	}
	decimals0, err := s.decimals(ctx, token0)
	if err != nil {
		return nil, err
	}
	decimals1, err := s.decimals(ctx, token1)
	if err != nil {
		return nil, err
	}
	layout := &poolLayout{baseIsToken0: token1 == baseUSDC, scale: math.Pow10(int(decimals0) - int(decimals1))}
	s.mu.Lock()
	s.layout = layout
	s.mu.Unlock()
	return layout, nil
}

func (s *PoolBandedAnchorSource) address(ctx context.Context, to common.Address, method string) (common.Address, error) {
	input, err := poolABI.Pack(method)
	if err != nil {
		return common.Address{}, err
	}
	raw, err := ethCall(ctx, s.client, s.rpcURL, to, input)
	if err != nil {
		return common.Address{}, fmt.Errorf("pool %s %s: %w", to.Hex(), method, err)
	}
	values, err := poolABI.Unpack(method, raw)
	if err != nil || len(values) != 1 {
		return common.Address{}, fmt.Errorf("pool %s %s: decode: %v", to.Hex(), method, err)
	}
	address, ok := values[0].(common.Address)
	if !ok {
		return common.Address{}, fmt.Errorf("pool %s %s: unexpected type %T", to.Hex(), method, values[0])
	}
	return address, nil
}

func (s *PoolBandedAnchorSource) decimals(ctx context.Context, token common.Address) (uint8, error) {
	input, err := cngnOracleABI.Pack("decimals") // the same decimals() as an ERC-20's
	if err != nil {
		return 0, err
	}
	raw, err := ethCall(ctx, s.client, s.rpcURL, token, input)
	if err != nil {
		return 0, fmt.Errorf("token %s decimals: %w", token.Hex(), err)
	}
	values, err := cngnOracleABI.Unpack("decimals", raw)
	if err != nil || len(values) != 1 {
		return 0, fmt.Errorf("token %s decimals: decode: %v", token.Hex(), err)
	}
	value, ok := values[0].(uint8)
	if !ok {
		return 0, fmt.Errorf("token %s decimals: unexpected type %T", token.Hex(), values[0])
	}
	return value, nil
}

// poolABI is the slice of a Uniswap v3 / Aerodrome Slipstream pool the reference reads. Selectors,
// checked with cast sig: observe(uint32[]) 0x883bdbfd, token0() 0x0dfe1681, token1() 0xd21220a7
// (pinned by TestPoolReferenceSelectors).
var poolABI = mustParseABI(`[
{"name":"observe","type":"function","stateMutability":"view","inputs":[{"name":"secondsAgos","type":"uint32[]"}],"outputs":[
  {"name":"tickCumulatives","type":"int56[]"},
  {"name":"secondsPerLiquidityCumulativeX128s","type":"uint160[]"}]},
{"name":"token0","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"address"}]},
{"name":"token1","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"address"}]}
]`)
