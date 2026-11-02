package config

import "fmt"

// OptionConfig sizes and prices the options quoter. Sizes are in the base (cNGN), prices in USDC per cNGN.
type OptionConfig struct {
	// HalfSpreadVol is each side's distance from the surface vol, in vol (0.03 is 3 vol points): bid at vol - h, ask at
	// vol + h, so the spread scales with each strike's vega rather than its premium.
	HalfSpreadVol float64
	// MinHalfSpread is the least each side sits from fair, in premium (USDC per cNGN), for strikes whose vega is tiny.
	MinHalfSpread float64
	// OrderSize is each side's size per strike, cNGN, rounded down to the venue unit (1,000 cNGN).
	OrderSize float64
	// MaxPosition bounds the position per strike either way, cNGN: a side that would pass it is cut to the room left.
	MaxPosition float64
	// IMUsage is the share of the account's IM surplus a written side may use, in (0, 1].
	IMUsage float64
	// CutoffSeconds stops quoting this long before the series' last trade time.
	CutoffSeconds int64
	// DegradedSpreadMultiplier widens both sides while the vol publisher reports degraded confidence (FX stress); 0
	// pulls the strike instead.
	DegradedSpreadMultiplier float64
	// DeltaSkewBPS shifts the forward the quotes are priced at by up to this many bps against the process's net delta,
	// reaching it at DeltaFullAt cNGN of delta: long delta lowers calls and raises puts, so fills reduce it.
	DeltaSkewBPS float64
	DeltaFullAt  float64
}

func loadOptionConfig() OptionConfig {
	return OptionConfig{
		HalfSpreadVol:            envFloat("MM_OPTION_HALF_SPREAD_VOL", 0.03),
		MinHalfSpread:            envFloat("MM_OPTION_MIN_HALF_SPREAD", 0.0000005),
		OrderSize:                envFloat("MM_OPTION_ORDER_SIZE", 100_000),
		MaxPosition:              envFloat("MM_OPTION_MAX_POSITION", 200_000),
		IMUsage:                  envFloat("MM_OPTION_IM_USAGE", 0.5),
		CutoffSeconds:            int64(envInt("MM_OPTION_CUTOFF_SECONDS", 1800)),
		DegradedSpreadMultiplier: envFloat("MM_OPTION_DEGRADED_SPREAD_MULTIPLIER", 2),
		DeltaSkewBPS:             envFloat("MM_OPTION_DELTA_SKEW_BPS", 50),
		DeltaFullAt:              envFloat("MM_OPTION_DELTA_FULL_AT", 500_000),
	}
}

func (c OptionConfig) validate() error {
	switch {
	case c.HalfSpreadVol < 0 || c.HalfSpreadVol >= 0.5:
		return fmt.Errorf("MM_OPTION_HALF_SPREAD_VOL must be in [0, 0.5), got %v", c.HalfSpreadVol)
	case c.MinHalfSpread < 0:
		return fmt.Errorf("MM_OPTION_MIN_HALF_SPREAD must be >= 0, got %v", c.MinHalfSpread)
	case c.HalfSpreadVol == 0 && c.MinHalfSpread == 0:
		return fmt.Errorf("MM_OPTION_HALF_SPREAD_VOL and MM_OPTION_MIN_HALF_SPREAD are both 0: the quotes would sit on fair")
	case c.OrderSize <= 0 || c.MaxPosition <= 0:
		return fmt.Errorf("MM_OPTION_ORDER_SIZE and MM_OPTION_MAX_POSITION must be > 0")
	case c.IMUsage <= 0 || c.IMUsage > 1:
		return fmt.Errorf("MM_OPTION_IM_USAGE must be in (0, 1], got %v", c.IMUsage)
	case c.CutoffSeconds < 0:
		return fmt.Errorf("MM_OPTION_CUTOFF_SECONDS must be >= 0")
	case c.DegradedSpreadMultiplier != 0 && c.DegradedSpreadMultiplier < 1:
		return fmt.Errorf("MM_OPTION_DEGRADED_SPREAD_MULTIPLIER widens (>= 1) or pulls (0), got %v", c.DegradedSpreadMultiplier)
	case c.DeltaSkewBPS < 0 || c.DeltaSkewBPS > 1000 || c.DeltaFullAt <= 0:
		return fmt.Errorf("MM_OPTION_DELTA_SKEW_BPS must be in [0, 1000] and MM_OPTION_DELTA_FULL_AT > 0")
	}
	return nil
}
