package exchange

import (
	"fmt"
	"math/big"
	"strings"
)

// spotAmountScale is the signed on-chain amount of one desired_amount unit on a spot market: one
// whole base unit of an 18-decimal escrow. PlaceLimitOrder signs floatToRaw(whole units), i.e. this.
var spotAmountScale = new(big.Int).Set(decimalScale)

// spotWiringError is CheckSpotWiring's comparison of an anchored spot listing against what the bot
// signs for, without the chain read: the configured TradeModule, that module's quoteAsset(), and
// the 1e18-per-unit amount the bot signs. A field the listing leaves empty is not checked.
func spotWiringError(spec MarketSpec, tradeModule, quoteAsset string) error {
	if spec.ListedTradeModule != "" && !strings.EqualFold(tradeModule, spec.ListedTradeModule) {
		return fmt.Errorf("MM_TRADE_MODULE_ADDRESS %s is not %s's trade module %s", tradeModule, spec.Symbol, spec.ListedTradeModule)
	}
	if spec.ListedQuoteAsset != "" && !strings.EqualFold(quoteAsset, spec.ListedQuoteAsset) {
		return fmt.Errorf("the module's quoteAsset %s is not %s's quote asset %s", quoteAsset, spec.Symbol, spec.ListedQuoteAsset)
	}
	if spec.AmountScale != "" {
		scale, ok := new(big.Int).SetString(spec.AmountScale, 10)
		if !ok || scale.Cmp(spotAmountScale) != 0 {
			return fmt.Errorf("%s lists amount_scale %q; the bot signs %s per whole %s", spec.Symbol, spec.AmountScale, spotAmountScale, baseUnitName(spec))
		}
	}
	if spec.AssetAddress == "" {
		return fmt.Errorf("%s lists no asset_address: the bot has no %s escrow to read its base balance from", spec.Symbol, baseUnitName(spec))
	}
	return nil
}

// baseUnitName is the base asset's symbol for messages, cNGN where a hand-built spec names none.
func baseUnitName(spec MarketSpec) string {
	if spec.BaseAsset != "" {
		return spec.BaseAsset
	}
	if spec.IsAnchoredSpot() {
		return "base unit"
	}
	return cngnSymbol
}
