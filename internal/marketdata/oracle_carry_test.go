package marketdata

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/numofx/market-maker/internal/config"
	"github.com/numofx/market-maker/internal/exchange"
)

func TestCarryFairValue(t *testing.T) {
	now := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)

	t.Run("applies continuous compounding to expiry", func(t *testing.T) {
		expiry := now.Add(365 * 24 * time.Hour).Unix()
		got := carryFairValue(1000, 0.08, expiry, now)
		want := 1000 * math.Exp(0.08)
		if math.Abs(got-want) > 1e-9 {
			t.Fatalf("fair = %v, want %v", got, want)
		}
	})

	t.Run("expired market clamps to spot", func(t *testing.T) {
		expiry := now.Add(-24 * time.Hour).Unix()
		if got := carryFairValue(1373.55, 0.08, expiry, now); got != 1373.55 {
			t.Fatalf("fair = %v, want spot", got)
		}
	})

	t.Run("missing expiry returns spot", func(t *testing.T) {
		if got := carryFairValue(1373.55, 0.08, 0, now); got != 1373.55 {
			t.Fatalf("fair = %v, want spot", got)
		}
	})

	t.Run("zero rate returns spot", func(t *testing.T) {
		expiry := now.Add(60 * 24 * time.Hour).Unix()
		if got := carryFairValue(1373.55, 0, expiry, now); got != 1373.55 {
			t.Fatalf("fair = %v, want spot", got)
		}
	})

	t.Run("sep16 shape matches expectation", func(t *testing.T) {
		// ~56 days at 8% APR on spot 1373.55 should land near 1390, not at spot.
		expiry := now.Add(56 * 24 * time.Hour).Unix()
		got := carryFairValue(1373.55, 0.08, expiry, now)
		if got < 1388 || got > 1393 {
			t.Fatalf("fair = %v, want ~1390", got)
		}
	})
}

type stubOracleFetcher struct {
	quote ExternalAnchorQuote
	calls int
}

func (s *stubOracleFetcher) Fetch(context.Context) ExternalAnchorQuote {
	s.calls++
	return s.quote
}

func TestOracleCarryAnchorSource(t *testing.T) {
	t.Run("returns carry-adjusted price", func(t *testing.T) {
		expiry := time.Now().Add(56 * 24 * time.Hour).Unix()
		source := &OracleCarryAnchorSource{
			fetcher:    &stubOracleFetcher{quote: ExternalAnchorQuote{Price: 1373.55, Present: true, FetchedAt: time.Now()}},
			rateAPR:    0.08,
			expiryUnix: expiry,
			maxAge:     time.Hour,
		}
		price, err := source.GetAnchorPrice(context.Background(), "USDCcNGN-SEP16-2026")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if price <= 1373.55 {
			t.Fatalf("price = %v, want above spot (carry applied)", price)
		}
	})

	t.Run("rejects stale oracle data", func(t *testing.T) {
		source := &OracleCarryAnchorSource{
			fetcher: &stubOracleFetcher{quote: ExternalAnchorQuote{Price: 1373.55, Present: true, FetchedAt: time.Now().Add(-2 * time.Hour)}},
			maxAge:  time.Hour,
		}
		if _, err := source.GetAnchorPrice(context.Background(), "USDCcNGN-SEP16-2026"); err == nil {
			t.Fatal("expected stale-oracle error")
		}
	})

	t.Run("rejects missing oracle data", func(t *testing.T) {
		source := &OracleCarryAnchorSource{fetcher: &stubOracleFetcher{}}
		if _, err := source.GetAnchorPrice(context.Background(), "USDCcNGN-SEP16-2026"); err == nil {
			t.Fatal("expected unavailable-oracle error")
		}
	})

	t.Run("throttles repeat fetches", func(t *testing.T) {
		fetcher := &stubOracleFetcher{quote: ExternalAnchorQuote{Price: 1373.55, Present: true, FetchedAt: time.Now()}}
		source := &OracleCarryAnchorSource{fetcher: fetcher, maxAge: time.Hour, refreshInterval: time.Minute}
		for i := 0; i < 5; i++ {
			if _, err := source.GetAnchorPrice(context.Background(), "x"); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		}
		if fetcher.calls != 1 {
			t.Fatalf("fetcher calls = %d, want 1 (throttled)", fetcher.calls)
		}
	})
}

func TestNewAnchorSourceOracleCarry(t *testing.T) {
	cfg := config.Config{
		AnchorSourceType:   "oracle_carry",
		RPCURL:             "http://127.0.0.1:0",
		CarryRateAPR:       0.08,
		AnchorOracleMaxAge: 4 * time.Hour,
	}
	spec := exchange.MarketSpec{Symbol: "USDCcNGN-SEP16-2026", ExpiryTimestamp: 1789567201}
	source := NewAnchorSource(cfg, spec)
	if source.Name() != "oracle_carry" {
		t.Fatalf("source name = %q, want oracle_carry", source.Name())
	}
	carry, ok := source.(*OracleCarryAnchorSource)
	if !ok {
		t.Fatalf("source type = %T", source)
	}
	if carry.expiryUnix != 1789567201 {
		t.Fatalf("expiry = %d", carry.expiryUnix)
	}
	if fmt.Sprintf("%.2f", carry.rateAPR) != "0.08" {
		t.Fatalf("rate = %v", carry.rateAPR)
	}
}
