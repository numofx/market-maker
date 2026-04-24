package exchange

import (
	"strings"
	"testing"
)

func TestMachineCancelReason(t *testing.T) {
	got := machineCancelReason("loyal-flexibility", "stale_or_wrong")
	want := "bot.loyal-flexibility.stale_or_wrong"
	if got != want {
		t.Fatalf("machineCancelReason() = %q want %q", got, want)
	}
}

func TestIsProtectedOrderID(t *testing.T) {
	if !isProtectedOrderID("validation:apr:1", []string{"validation:", "test:"}) {
		t.Fatal("expected validation prefix to be protected")
	}
	if isProtectedOrderID("mm:USDCcNGN-APR30-2026:buy:1", []string{"validation:", "test:"}) {
		t.Fatal("unexpected protection for normal mm order id")
	}
}

func TestAssetCodeReadinessError(t *testing.T) {
	checks := []AssetCodeCheck{
		{EnvVar: "CNGN_SPOT_ASSET_ADDRESS", Address: "0xe4b6e05b9910ab08a947a20faecc4524bf8a7f7e", HasCode: false},
		{EnvVar: "TRADE_MODULE_QUOTE_ASSET", Address: "0x1917960763bf3a0dfa10a05f0a112e828c1a934f", HasCode: true},
	}
	err := assetCodeReadinessError(checks)
	if err == nil {
		t.Fatal("expected readiness error")
	}
	if !strings.Contains(err.Error(), "token_address_has_no_code") || !strings.Contains(err.Error(), "CNGN_SPOT_ASSET_ADDRESS") {
		t.Fatalf("unexpected error: %v", err)
	}
	checks[0].HasCode = true
	if err := assetCodeReadinessError(checks); err != nil {
		t.Fatalf("unexpected readiness error: %v", err)
	}
}

func TestAssetAddressEnvVar(t *testing.T) {
	if got := assetAddressEnvVar(MarketSpec{Symbol: "USDCcNGN-SPOT"}); got != "CNGN_SPOT_ASSET_ADDRESS" {
		t.Fatalf("spot env var = %q", got)
	}
	if got := assetAddressEnvVar(MarketSpec{Symbol: "USDCcNGN-APR30-2026"}); got != "MARKET_ASSET_ADDRESS" {
		t.Fatalf("generic env var = %q", got)
	}
}
