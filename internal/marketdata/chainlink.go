package marketdata

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// ErrAnchorStale and ErrAnchorInvalid mark an anchor read that succeeded and returned a price the
// bot must not quote around: a round older than its max age, or a non-positive answer. The bot halts
// on either at once and cancels its quotes (handleLoadError), with no MM_STALE_ANCHOR_TIMEOUT_SECONDS
// grace: the grace is for an RPC that blips, and this is the feed itself saying it has no price.
var (
	ErrAnchorStale   = errors.New("anchor data stale")
	ErrAnchorInvalid = errors.New("anchor data invalid")
)

// chainlinkRefreshThrottle bounds how often the feed is re-read; the 2s quote loop is served the
// last round in between. The round's age is re-checked on every call, cached or not.
const chainlinkRefreshThrottle = 15 * time.Second

// ChainlinkAnchorSource is MM_ANCHOR_SOURCE_TYPE=chainlink: a Chainlink AggregatorV3 feed read over
// JSON-RPC (MM_RPC_URL, through the same eth_call client as the cNGN oracle), priced answer /
// 10^decimals. It is not inverted or converted: the feed must be quoted in the market's own price
// unit, as EURC/USD is for EURC-USDC (USD per EURC, read as USDC per EURC).
//
// It refuses a non-positive answer (ErrAnchorInvalid) and a round whose updatedAt is older than
// maxAge or unset (ErrAnchorStale). An RPC failure is a plain error, which the stale-anchor grace
// (MM_STALE_ANCHOR_TIMEOUT_SECONDS) handles like any other anchor's.
type ChainlinkAnchorSource struct {
	rpcURL  string
	feed    common.Address
	maxAge  time.Duration
	client  *http.Client
	now     func() time.Time
	refresh time.Duration

	mu        sync.Mutex
	decimals  *uint8
	price     float64
	updatedAt time.Time
	readAt    time.Time
}

func NewChainlinkAnchorSource(rpcURL, feed string, maxAge time.Duration) *ChainlinkAnchorSource {
	return &ChainlinkAnchorSource{
		rpcURL:  rpcURL,
		feed:    common.HexToAddress(feed),
		maxAge:  maxAge,
		client:  &http.Client{Timeout: 5 * time.Second},
		now:     time.Now,
		refresh: chainlinkRefreshThrottle,
	}
}

func (s *ChainlinkAnchorSource) Name() string { return "chainlink" }

func (s *ChainlinkAnchorSource) GetAnchorPrice(ctx context.Context, _ string) (float64, error) {
	now := s.now()
	s.mu.Lock()
	cached := s.price > 0 && s.refresh > 0 && now.Sub(s.readAt) < s.refresh
	price, updatedAt := s.price, s.updatedAt
	s.mu.Unlock()

	if !cached {
		var err error
		price, updatedAt, err = s.readRound(ctx)
		if err != nil {
			return 0, err
		}
		s.mu.Lock()
		s.price, s.updatedAt, s.readAt = price, updatedAt, now
		s.mu.Unlock()
	}
	if age := now.Sub(updatedAt); age > s.maxAge {
		return 0, fmt.Errorf("%w: chainlink feed %s last updated %s ago (updatedAt %s), max age %s", ErrAnchorStale, s.feed.Hex(), age.Round(time.Second), updatedAt.Format(time.RFC3339), s.maxAge)
	}
	return price, nil
}

// readRound reads latestRoundData and scales the answer by the feed's decimals, exactly (big.Rat),
// refusing a round the bot must not quote around.
func (s *ChainlinkAnchorSource) readRound(ctx context.Context) (float64, time.Time, error) {
	decimals, err := s.feedDecimals(ctx)
	if err != nil {
		return 0, time.Time{}, err
	}
	input, err := cngnOracleABI.Pack("latestRoundData")
	if err != nil {
		return 0, time.Time{}, err
	}
	raw, err := ethCall(ctx, s.client, s.rpcURL, s.feed, input)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("chainlink feed %s latestRoundData: %w", s.feed.Hex(), err)
	}
	values, err := cngnOracleABI.Unpack("latestRoundData", raw)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("chainlink feed %s latestRoundData: decode: %w", s.feed.Hex(), err)
	}
	if len(values) != 5 {
		return 0, time.Time{}, fmt.Errorf("chainlink feed %s latestRoundData: %d outputs, want 5", s.feed.Hex(), len(values))
	}
	answer, ok := values[1].(*big.Int)
	if !ok || answer == nil {
		return 0, time.Time{}, fmt.Errorf("chainlink feed %s: unexpected answer type %T", s.feed.Hex(), values[1])
	}
	updated, ok := values[3].(*big.Int)
	if !ok || updated == nil {
		return 0, time.Time{}, fmt.Errorf("chainlink feed %s: unexpected updatedAt type %T", s.feed.Hex(), values[3])
	}
	if answer.Sign() <= 0 {
		return 0, time.Time{}, fmt.Errorf("%w: chainlink feed %s answered %s", ErrAnchorInvalid, s.feed.Hex(), answer)
	}
	if updated.Sign() <= 0 || !updated.IsInt64() {
		return 0, time.Time{}, fmt.Errorf("%w: chainlink feed %s round has updatedAt %s", ErrAnchorStale, s.feed.Hex(), updated)
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	price, _ := new(big.Rat).SetFrac(answer, scale).Float64()
	if !(price > 0) {
		return 0, time.Time{}, fmt.Errorf("%w: chainlink feed %s answered %s at %d decimals", ErrAnchorInvalid, s.feed.Hex(), answer, decimals)
	}
	return price, time.Unix(updated.Int64(), 0).UTC(), nil
}

// feedDecimals reads decimals() once; a feed's decimals never change.
func (s *ChainlinkAnchorSource) feedDecimals(ctx context.Context) (uint8, error) {
	s.mu.Lock()
	if s.decimals != nil {
		value := *s.decimals
		s.mu.Unlock()
		return value, nil
	}
	s.mu.Unlock()
	input, err := cngnOracleABI.Pack("decimals")
	if err != nil {
		return 0, err
	}
	raw, err := ethCall(ctx, s.client, s.rpcURL, s.feed, input)
	if err != nil {
		return 0, fmt.Errorf("chainlink feed %s decimals: %w", s.feed.Hex(), err)
	}
	values, err := cngnOracleABI.Unpack("decimals", raw)
	if err != nil || len(values) != 1 {
		return 0, fmt.Errorf("chainlink feed %s decimals: decode: %v", s.feed.Hex(), err)
	}
	value, ok := values[0].(uint8)
	if !ok {
		return 0, fmt.Errorf("chainlink feed %s: unexpected decimals type %T", s.feed.Hex(), values[0])
	}
	s.mu.Lock()
	s.decimals = &value
	s.mu.Unlock()
	return value, nil
}
