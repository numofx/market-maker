package onchainbook

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

// maxRegions is the contract's limit on a curve (OnchainBook.setCurve).
const maxRegions = 10

// ParseCurve reads "lots@offsetTicks,..." (MM_BOOK_CURVE), e.g. "200@0,400@50": 200 lots at the best level, 400
// more 50 ticks behind it. The contract's rules are checked here so a bad value fails at startup, not on chain.
func ParseCurve(spec string) ([]Region, error) {
	var regions []Region
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		lots, offset, ok := strings.Cut(item, "@")
		if !ok {
			return nil, fmt.Errorf("curve region %q: want lots@offsetTicks", item)
		}
		l, err := strconv.ParseUint(strings.TrimSpace(lots), 10, 32)
		if err != nil || l == 0 {
			return nil, fmt.Errorf("curve region %q: lots must be a positive integer", item)
		}
		o, err := strconv.ParseUint(strings.TrimSpace(offset), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("curve region %q: offset must be a non-negative integer", item)
		}
		if n := len(regions); n > 0 && uint32(o) < regions[n-1].OffsetTicks {
			return nil, fmt.Errorf("curve region %q: offsets must not decrease", item)
		}
		regions = append(regions, Region{Lots: uint32(l), OffsetTicks: uint32(o)})
	}
	if len(regions) == 0 || len(regions) > maxRegions {
		return nil, fmt.Errorf("curve %q: 1 to %d regions", spec, maxRegions)
	}
	return regions, nil
}

// allowanceFloor is what the runbook grants (type(uint).max); anything below 2^128 is treated as not granted, so a
// partial allowance that the book would spend down to "too low" mid-session is caught here instead.
var allowanceFloor = new(big.Int).Lsh(big.NewInt(1), 128)

// Preflight checks everything the book will check at registration and at every take, so the bot refuses to start
// rather than quote a spline the book skips. It sets the curve and registers the maker when needed (in dry run it
// only reports that it would); it never moves funds or grants allowances: those are the runbook's.
func Preflight(ctx context.Context, c *Chain, owner common.Address, p Params, curveID uint16, curve []Region,
	referenceMarketID uint64, dryRun bool, log *slog.Logger) (Market, Feeds, error) {
	m, err := c.Market(ctx)
	if err != nil {
		return Market{}, Feeds{}, err
	}
	if uint32(p.TTL.Seconds()) > m.MaxTTL {
		return m, Feeds{}, fmt.Errorf("ttl %s over the market's cap of %ds", p.TTL, m.MaxTTL)
	}
	if p.SelfPriced {
		if !c.V2 {
			return m, Feeds{}, ErrSelfPricedOnV1
		}
		cap, err := c.SelfPricedTTLCap(ctx)
		if err != nil {
			return m, Feeds{}, err
		}
		if cap == 0 {
			return m, Feeds{}, errors.New("self-priced splines are off on this market (maxSelfPricedTtl 0: the vault's setSelfPricedTtlCap)")
		}
		if uint32(p.TTL.Seconds()) > cap {
			return m, Feeds{}, fmt.Errorf("ttl %s over the market's self-priced cap of %ds", p.TTL, cap)
		}
	}
	if p.BidCurve != curveID || p.AskCurve != curveID {
		return m, Feeds{}, errors.New("both sides quote the configured curve id")
	}

	accOwner, manager, err := c.AccountOwnerAndManager(ctx)
	if err != nil {
		return m, Feeds{}, err
	}
	var problems []string
	if accOwner != owner {
		problems = append(problems, fmt.Sprintf("account %s is owned by %s, not the signer %s (an account Matching holds cannot quote here)",
			c.AccountID, accOwner.Hex(), owner.Hex()))
	}
	if manager != m.Engine {
		problems = append(problems, fmt.Sprintf("account %s is under manager %s, not the market's engine %s", c.AccountID, manager.Hex(), m.Engine.Hex()))
	}
	if ok, err := c.MakerAllowed(ctx, owner); err != nil || !ok {
		problems = append(problems, fmt.Sprintf("the book does not allow %s as a maker (the vault's setup batch, setMakerAllowed) %v", owner.Hex(), errOrEmpty(err)))
	}
	cash, err := c.Cash(ctx, m.Engine)
	if err != nil {
		return m, Feeds{}, fmt.Errorf("engine cash: %w", err)
	}
	for _, need := range []struct {
		what     string
		asset    common.Address
		positive bool
	}{{"future, buying", m.Future, true}, {"future, selling", m.Future, false}, {"cash, paying", cash, false}} {
		a, err := c.Allowance(ctx, owner, need.asset, need.positive)
		if err != nil {
			return m, Feeds{}, fmt.Errorf("allowance (%s): %w", need.what, err)
		}
		if a.Cmp(allowanceFloor) < 0 {
			problems = append(problems, fmt.Sprintf("allowance to the book (%s) is %s: grant type(uint).max (runbook)", need.what, a))
		}
	}
	if len(problems) > 0 {
		return m, Feeds{}, errors.New(strings.Join(problems, "; "))
	}

	feeds, err := c.ResolveFeeds(ctx, m, referenceMarketID)
	if err != nil {
		return m, Feeds{}, err
	}

	curves := []struct {
		id      uint16
		regions []Region
	}{{curveID, curve}}
	if p.MaxPositionLots > 0 {
		if err := checkParkOutsideBand(ctx, c, m, feeds, p); err != nil {
			return m, feeds, err
		}
		curves = append(curves, struct {
			id      uint16
			regions []Region
		}{p.ParkCurve, ParkCurveRegions()})
	}
	for _, want := range curves {
		stored, err := c.Curve(ctx, owner, want.id)
		if err != nil {
			return m, feeds, err
		}
		if sameCurve(stored, want.regions) {
			continue
		}
		if dryRun {
			log.Warn("curve differs: would setCurve (dry run)", "curve_id", want.id, "stored", stored, "configured", want.regions)
			continue
		}
		hash, err := c.SetCurve(ctx, want.id, want.regions)
		if err != nil {
			return m, feeds, fmt.Errorf("setCurve(%d): %w", want.id, err)
		}
		log.Info("curve set", "curve_id", want.id, "regions", want.regions, "tx", hash.Hex())
	}
	registered, err := c.IsRegistered(ctx)
	if err != nil {
		return m, feeds, err
	}
	if !registered {
		if dryRun {
			log.Warn("not registered: would registerMaker (dry run)", "account", c.AccountID)
		} else {
			hash, err := c.Register(ctx)
			if err != nil {
				return m, feeds, fmt.Errorf("registerMaker: %w", err)
			}
			log.Info("maker registered", "account", c.AccountID, "tx", hash.Hex())
		}
	}
	log.Info("preflight ok", "book_v2", c.V2, "self_priced", p.SelfPriced, "market_enabled", m.Enabled, "future", m.Future.Hex(), "expiry", m.Expiry,
		"last_trade", m.LastTradeTime.UTC(), "tick", m.TickSize, "lot", m.LotSize, "index", feeds.Index.Hex(),
		"forward", feeds.Forward.Hex(), "reference", feeds.Reference.Hex())
	return m, feeds, nil
}

// checkParkOutsideBand refuses a cap whose park curve the book could fill: the parked level, ParkOffsetTicks plus the
// half-spread from the mid, must be at least twice the index band away. A market with no band (0) fills anywhere.
func checkParkOutsideBand(ctx context.Context, c *Chain, m Market, feeds Feeds, p Params) error {
	band, err := c.IndexBand(ctx, m)
	if err != nil {
		return err
	}
	if band.Sign() == 0 {
		return errors.New("position cap: the series has no index band, so a parked side could fill")
	}
	index, _, _, errs := c.Prices(ctx, feeds, m.Expiry)
	if index == nil || index.Sign() <= 0 {
		return fmt.Errorf("position cap: index unreadable to check the park curve against the band %v", errs)
	}
	if !parkOutsideBand(band, index, m.TickSize, p.HalfSpreadTicks) {
		return fmt.Errorf("position cap: the park curve (%d ticks out) is inside twice the index band %s at index %s",
			ParkOffsetTicks, band, index)
	}
	return nil
}

// parkOutsideBand: (ParkOffsetTicks + halfSpread) * tick >= 2 * band * index / 1e18.
func parkOutsideBand(band, index, tick *big.Int, halfSpread uint32) bool {
	dist := new(big.Int).Mul(big.NewInt(int64(ParkOffsetTicks)+int64(halfSpread)), tick)
	dist.Mul(dist, big.NewInt(1e18))
	need := new(big.Int).Mul(band, index)
	need.Mul(need, big.NewInt(2))
	return dist.Cmp(need) >= 0
}

func sameCurve(a, b []Region) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func errOrEmpty(err error) string {
	if err == nil {
		return ""
	}
	return "(" + err.Error() + ")"
}
