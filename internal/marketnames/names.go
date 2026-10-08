// Package marketnames is the bot's own knowledge of how the venue names its cNGN markets: the
// base-quote identifiers markets-service lists them under, and the inverted identifiers it listed
// them under before. It exists so that no other package compares a market name to a string literal.
//
// The venue's listing is the authority. Everything here is the fallback for a listing that carries
// no `aliases`: an old markets-service, which knows only the old names, or a new one that has not
// started publishing them.
package marketnames

const (
	// SpotCanonical and PerpCanonical are the identifiers markets-service lists the cNGN markets
	// under after the rename: cNGN is the base, priced in USDC per cNGN.
	SpotCanonical = "cNGN-USDC"
	PerpCanonical = "cNGN-PERP"
	// SpotLegacy and PerpLegacy are the identifiers the same markets were listed under before, kept
	// by the venue as deprecated, exact-match aliases.
	SpotLegacy = "USDCcNGN-SPOT"
	PerpLegacy = "USDCcNGN-PERP"
)

// fallback maps each known identifier to the other identifiers for the same market, in both
// directions, so it serves an old listing and a new one alike.
var fallback = map[string][]string{
	SpotCanonical: {SpotLegacy},
	SpotLegacy:    {SpotCanonical},
	PerpCanonical: {PerpLegacy},
	PerpLegacy:    {PerpCanonical},
}

// FallbackAliases returns the other identifiers the bot knows for a listed market name, for a
// listing that carries no aliases of its own. Nil for a market this package does not know.
func FallbackAliases(listed string) []string {
	return append([]string(nil), fallback[listed]...)
}

// IsSpot reports whether name is one of the identifiers of the cNGN spot market.
func IsSpot(name string) bool { return name == SpotCanonical || name == SpotLegacy }

// IsPerp reports whether name is one of the identifiers of the cNGN perp.
func IsPerp(name string) bool { return name == PerpCanonical || name == PerpLegacy }
