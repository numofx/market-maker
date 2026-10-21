package config

import (
	"strings"
	"testing"
	"time"
)

// The EURC-USDC maker's environment as infra writes it.
func setEURCEnv(t *testing.T) {
	t.Helper()
	setRequiredEnv(t)
	t.Setenv("MM_MARKET_SYMBOL", "EURC-USDC")
	t.Setenv("MM_ANCHOR_SOURCE_TYPE", "chainlink")
	t.Setenv("MM_ANCHOR_CHAINLINK_FEED", "0xDAe398520e2B67cd3f27aeF9Cf14D93D927f8250")
	t.Setenv("MM_ANCHOR_CHAINLINK_MAX_AGE_SECONDS", "")
	t.Setenv("MM_ANCHOR_CHAINLINK_RPC_URL", "")
	t.Setenv("MM_MAX_ANCHOR_DEVIATION_BPS", "50")
	t.Setenv("MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_ENABLED", "false")
}

func TestChainlinkAnchorConfig(t *testing.T) {
	setEURCEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AnchorSourceType != AnchorSourceChainlink || cfg.AnchorChainlinkMaxAge != 90000*time.Second {
		t.Fatalf("anchor %q max age %s, want chainlink and the 90000s default", cfg.AnchorSourceType, cfg.AnchorChainlinkMaxAge)
	}
	// Unset, the feed is read over MM_RPC_URL; set, over its own endpoint.
	if cfg.AnchorChainlinkRPCURL != "http://127.0.0.1:2" {
		t.Fatalf("rpc %q, want MM_RPC_URL", cfg.AnchorChainlinkRPCURL)
	}
	t.Setenv("MM_ANCHOR_CHAINLINK_RPC_URL", "http://127.0.0.1:3")
	if cfg, err = Load(); err != nil || cfg.AnchorChainlinkRPCURL != "http://127.0.0.1:3" {
		t.Fatalf("rpc %q err %v, want MM_ANCHOR_CHAINLINK_RPC_URL", cfg.AnchorChainlinkRPCURL, err)
	}
}

func TestChainlinkAnchorConfigRefusals(t *testing.T) {
	for name, tc := range map[string]struct{ key, value, msg string }{
		"no feed":               {"MM_ANCHOR_CHAINLINK_FEED", "", "MM_ANCHOR_CHAINLINK_FEED"},
		"not an address":        {"MM_ANCHOR_CHAINLINK_FEED", "eurc-usd", "MM_ANCHOR_CHAINLINK_FEED"},
		"zero address":          {"MM_ANCHOR_CHAINLINK_FEED", "0x0000000000000000000000000000000000000000", "MM_ANCHOR_CHAINLINK_FEED"},
		"no max age":            {"MM_ANCHOR_CHAINLINK_MAX_AGE_SECONDS", "0", "MM_ANCHOR_CHAINLINK_MAX_AGE_SECONDS"},
		"deviation guard off":   {"MM_MAX_ANCHOR_DEVIATION_BPS", "0", "MM_MAX_ANCHOR_DEVIATION_BPS"},
		"an unknown anchor":     {"MM_ANCHOR_SOURCE_TYPE", "pyth", "MM_ANCHOR_SOURCE_TYPE"},
		"cNGN bootstrap anchor": {"MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_ENABLED", "true", "MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_ENABLED"},
	} {
		t.Run(name, func(t *testing.T) {
			setEURCEnv(t)
			t.Setenv(tc.key, tc.value)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("err %v, want one naming %s", err, tc.msg)
			}
		})
	}
}
