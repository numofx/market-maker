package config

import (
	"strings"
	"testing"
)

// A throwaway key; only its parseability matters here.
const testPrivateKey = "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"

// setRequiredEnv sets the minimum Load demands and blanks every variable these tests are about, so a
// developer's shell cannot make a case pass or fail by accident. envString treats "" as unset.
func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("MM_API_BASE_URL", "http://127.0.0.1:1")
	t.Setenv("MM_RPC_URL", "http://127.0.0.1:2")
	t.Setenv("MM_DATABASE_URL", "postgres://localhost/test")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("MM_SUBACCOUNT_ID", "10")
	t.Setenv("MM_OWNER_PRIVATE_KEY", testPrivateKey)
	for _, key := range []string{
		"MM_SIGNER_PRIVATE_KEY", "MM_ORDER_EXPIRY_SECONDS", "MM_EXPIRY_REPLACE_MARGIN_SECONDS",
		"MM_SIGNER_BACKEND", "MM_KMS_KEY_ID", "MM_CONTROL_ADDR", "MM_CONTROL_TOKEN", "MM_OPERATOR_MODE",
		"MM_ANCHOR_SOURCE_TYPE", "MM_USDCCNGN_SPOT_EXTERNAL_ANCHOR_ENABLED", "MM_QUOTE_LEVELS", "MM_LOG_LEVEL",
	} {
		t.Setenv(key, "")
	}
}

func TestDefaultsRollQuotesOnAShortExpiry(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OrderExpirySeconds != 60 || cfg.ExpiryReplaceMarginSeconds != 15 {
		t.Fatalf("expiry %d / margin %d, want 60 / 15", cfg.OrderExpirySeconds, cfg.ExpiryReplaceMarginSeconds)
	}
	if cfg.ControlAddr != "127.0.0.1:8081" || cfg.ControlToken != "" {
		t.Fatalf("control addr %q token %q, want loopback and disabled", cfg.ControlAddr, cfg.ControlToken)
	}
	if cfg.SignerBackend != SignerBackendLocal {
		t.Fatalf("signer backend %q, want local", cfg.SignerBackend)
	}
}

// Zero lets quotes lapse before they are replaced; half the expiry or more replaces a quote before
// it has rested as long as it is being replaced for. Both are refused at load, not clamped.
func TestExpiryReplaceMarginIsBounded(t *testing.T) {
	cases := []struct {
		expiry, margin string
		ok             bool
	}{
		{"60", "15", true},
		{"60", "29", true},
		{"60", "30", false},
		{"60", "0", false},
		{"60", "-1", false},
		{"61", "30", true},
		{"0", "15", false},
	}
	for _, tc := range cases {
		t.Run("expiry="+tc.expiry+"/margin="+tc.margin, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv("MM_ORDER_EXPIRY_SECONDS", tc.expiry)
			t.Setenv("MM_EXPIRY_REPLACE_MARGIN_SECONDS", tc.margin)
			_, err := Load()
			if tc.ok && err != nil {
				t.Fatalf("Load: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("expected Load to refuse the margin")
			}
		})
	}
}

// The point of KMS is that no key is in the environment, so requiring one would defeat it.
func TestKMSBackendDoesNotRequireAPrivateKey(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("MM_OWNER_PRIVATE_KEY", "")
	t.Setenv("MM_SIGNER_BACKEND", "kms")
	t.Setenv("MM_KMS_KEY_ID", "alias/mm-bot")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SignerBackend != SignerBackendKMS || cfg.KMSKeyID != "alias/mm-bot" {
		t.Fatalf("backend %q key %q", cfg.SignerBackend, cfg.KMSKeyID)
	}
}

func TestSignerBackendValidation(t *testing.T) {
	t.Run("kms needs a key id", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("MM_SIGNER_BACKEND", "kms")
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "MM_KMS_KEY_ID") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("local still needs the owner key", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("MM_OWNER_PRIVATE_KEY", "")
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "MM_OWNER_PRIVATE_KEY") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unknown backend", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("MM_SIGNER_BACKEND", "hsm")
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "MM_SIGNER_BACKEND") {
			t.Fatalf("err = %v", err)
		}
	})
}
