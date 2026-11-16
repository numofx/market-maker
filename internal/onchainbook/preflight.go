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

	stored, err := c.Curve(ctx, owner, curveID)
	if err != nil {
		return m, feeds, err
	}
	if !sameCurve(stored, curve) {
		if dryRun {
			log.Warn("curve differs: would setCurve (dry run)", "curve_id", curveID, "stored", stored, "configured", curve)
		} else {
			hash, err := c.SetCurve(ctx, curveID, curve)
			if err != nil {
				return m, feeds, fmt.Errorf("setCurve: %w", err)
			}
			log.Info("curve set", "curve_id", curveID, "regions", curve, "tx", hash.Hex())
		}
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
	log.Info("preflight ok", "market_enabled", m.Enabled, "future", m.Future.Hex(), "expiry", m.Expiry,
		"last_trade", m.LastTradeTime.UTC(), "tick", m.TickSize, "lot", m.LotSize, "index", feeds.Index.Hex(),
		"forward", feeds.Forward.Hex(), "reference", feeds.Reference.Hex())
	return m, feeds, nil
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
