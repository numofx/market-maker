package exchange

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
)

// Orientation is how markets-service presents a cNGN market relative to its engine.
//
// The engine never changes: it trades cNGN (the base) against USDC (the quote), prices in USDC per
// cNGN (~0.00073), sizes in whole cNGN, and a buy is a buy of cNGN. The bot's internal model is the
// engine's, on every market and on every code path. What varies is the venue's PUBLIC contract,
// named by each market's order_entry_spec:
//
//   - cngn_usdc_*_v1 presents the engine as it is. ui_intent is the engine order and the perp's
//     mark_price_ui / index_price_ui (and a position's liquidation_price_ui) are USDC per cNGN.
//   - usdc_cngn_*_v1, what production served until the cutover, presents the inverse: prices in cNGN
//     per USDC, sizes in USDC notional, and the side flipped so that BUY acquires USDC. Its ui_intent
//     and *_ui prices have to be converted on the way in.
//
// This file is the only place that knows about the inverted presentation. Nothing past it sees a
// cNGN-per-USDC number.
type Orientation string

const (
	// OrientationEngine: the venue presents prices in USDC per cNGN, sizes in cNGN, engine sides.
	OrientationEngine Orientation = "engine"
	// OrientationInverted: the venue presents prices in cNGN per USDC, sizes in USDC, sides flipped.
	OrientationInverted Orientation = "inverted"
)

// The order-entry specs markets-service has published for the cNGN markets. The cngn_usdc specs are
// the identity mapping; the usdc_cngn specs are the inverted presentation they replaced.
const (
	SpecCNGNUSDCSpot = "cngn_usdc_spot_v1"
	SpecCNGNUSDCPerp = "cngn_usdc_perp_v1"
	SpecUSDCCNGNSpot = "usdc_cngn_spot_v1"
	SpecUSDCCNGNPerp = "usdc_cngn_perp_v1"
)

// OrientationForSpec maps an order_entry_spec to the presentation it names. ok is false for a spec
// this build does not know, including an empty one.
func OrientationForSpec(spec string) (Orientation, bool) {
	switch strings.TrimSpace(spec) {
	case SpecCNGNUSDCSpot, SpecCNGNUSDCPerp:
		return OrientationEngine, true
	case SpecUSDCCNGNSpot, SpecUSDCCNGNPerp:
		return OrientationInverted, true
	default:
		return OrientationEngine, false
	}
}

var unknownSpecLogged sync.Map

// orientationOf resolves a market row's presentation. A spec this build does not know is read as the
// engine's own orientation -- the venue's stated direction of travel -- and said so once per market,
// because a silent guess here would be a ladder quoted 1.8 million times off.
func orientationOf(symbol, spec string) Orientation {
	o, ok := OrientationForSpec(spec)
	if !ok {
		key := symbol + "|" + spec
		if _, seen := unknownSpecLogged.LoadOrStore(key, struct{}{}); !seen {
			slog.Warn(
				"unknown order_entry_spec; assuming engine orientation",
				"market", symbol,
				"order_entry_spec", spec,
				"assumption", "prices USDC per cNGN, sizes cNGN, buy = buy cNGN",
			)
		}
	}
	return o
}

// EnginePrice is a venue-presented price in the engine's USDC per cNGN.
func (o Orientation) EnginePrice(uiPrice float64) (float64, error) {
	if !(uiPrice > 0) {
		return 0, fmt.Errorf("ui price must be positive, got %v", uiPrice)
	}
	if o == OrientationInverted {
		return 1 / uiPrice, nil
	}
	return uiPrice, nil
}

// EngineOrder is a venue-presented order (ui_intent) as the engine sees it: side as the engine
// matches it, price in USDC per cNGN, size in cNGN. Under the inverted presentation the size is the
// USDC notional, so the cNGN amount is size x price (cNGN per USDC); the side flips because the
// venue's BUY acquired USDC, which is the engine's sell of cNGN.
func (o Orientation) EngineOrder(side Side, uiPrice, uiSize float64) (Side, float64, float64, error) {
	if !(uiPrice > 0) || !(uiSize > 0) {
		return "", 0, 0, fmt.Errorf("ui price and size must be positive, got %v x %v", uiPrice, uiSize)
	}
	if side != SideBuy && side != SideSell {
		return "", 0, 0, fmt.Errorf("invalid side %q", side)
	}
	if o != OrientationInverted {
		return side, uiPrice, uiSize, nil
	}
	engineSide := SideBuy
	if side == SideBuy {
		engineSide = SideSell
	}
	return engineSide, 1 / uiPrice, uiSize * uiPrice, nil
}

// EngineOrderFromUIIntent decodes a spot_contract row (its spec and ui_intent strings, as /v1/book
// and /v1/trades carry them) into engine terms, keyed on the ROW's own spec rather than the
// market's: a tape can hold trades presented under both contracts across a cutover.
func EngineOrderFromUIIntent(spec, side, price, size string) (Side, float64, float64, error) {
	uiPrice, err := strconv.ParseFloat(strings.TrimSpace(price), 64)
	if err != nil {
		return "", 0, 0, fmt.Errorf("parse ui_intent price %q: %w", price, err)
	}
	uiSize, err := strconv.ParseFloat(strings.TrimSpace(size), 64)
	if err != nil {
		return "", 0, 0, fmt.Errorf("parse ui_intent size %q: %w", size, err)
	}
	o, _ := OrientationForSpec(spec)
	return o.EngineOrder(Side(strings.ToLower(strings.TrimSpace(side))), uiPrice, uiSize)
}

// EngineSymbols is the base and quote symbol in engine order. The inverted presentation lists USDC
// as the base and cNGN as the quote; the engine's base is cNGN.
func (o Orientation) EngineSymbols(base, quote string) (string, string) {
	if o == OrientationInverted {
		return quote, base
	}
	return base, quote
}

// perpPriceFromVenue is a perp price from /v1/markets in USDC per cNGN. The engine field
// (mark_price / index_price) is already engine-oriented under both contracts and is preferred; the
// *_ui field is converted through the market's orientation when the engine field is absent.
func perpPriceFromVenue(engineRaw, uiRaw string, o Orientation) float64 {
	if v := parseFloatOrZero(engineRaw); v > 0 {
		return v
	}
	ui := parseFloatOrZero(uiRaw)
	if ui <= 0 {
		return 0
	}
	price, err := o.EnginePrice(ui)
	if err != nil {
		return 0
	}
	return price
}
