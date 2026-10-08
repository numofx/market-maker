package logging

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

const (
	key    = "mUUT2RaigfrMafvzIwbCK-not-a-real-key"
	rpcURL = "https://base-mainnet.g.alchemy.com/v2/" + key
	dbURL  = "postgres://mm:s3cretpass@db.internal:5432/mm?sslmode=require"
)

func TestHostOnlyKeepsSchemeAndHostOnly(t *testing.T) {
	cases := map[string]string{
		rpcURL:                               "https://base-mainnet.g.alchemy.com",
		rpcURL + "/":                         "https://base-mainnet.g.alchemy.com",
		"https://x.quiknode.pro/abc/?k=v#f":  "https://x.quiknode.pro",
		"http://127.0.0.1:8545/key":          "http://127.0.0.1:8545",
		dbURL:                                "postgres://db.internal:5432",
		"wss://api.numofx.com/v1/ws?token=1": "wss://api.numofx.com",
		"":                                   "",
		"  ":                                 "",
		"not a url":                          "<redacted-url>",
		"://bad":                             "<redacted-url>",
	}
	for raw, want := range cases {
		if got := HostOnly(raw); got != want {
			t.Errorf("HostOnly(%q) = %q, want %q", raw, got, want)
		}
	}
}

// Every way a URL reaches a log record: the message, a string attr, an error attr (the shape
// net/http gives transport failures), a wrapped error, a group, a []string, pre-set attrs, and a
// group added with WithGroup. None may carry the key or the password.
func TestRedactingHandlerStripsConfiguredURLsEverywhere(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewRedactingHandler(slog.NewTextHandler(&buf, nil), rpcURL, dbURL, "", "https://host.only"))

	transportErr := fmt.Errorf("check base_asset code at 0xabc: %w", errors.New(`Post "`+rpcURL+`": dial tcp: i/o timeout`))
	logger.Info("rpc at "+rpcURL+" is slow",
		"rpc_label", rpcURL,
		"error", transportErr,
		"urls", []string{rpcURL + "/", dbURL},
		slog.Group("nested", "db", dbURL, "depth", 2),
		"count", 3,
	)
	logger.With("preset", rpcURL).WithGroup("g").Warn("db failed", "error", errors.New("connect "+dbURL+": refused"))

	out := buf.String()
	for _, secret := range []string{key, "s3cretpass", "/v2/", "mm:s3cretpass@"} {
		if strings.Contains(out, secret) {
			t.Fatalf("log output leaks %q:\n%s", secret, out)
		}
	}
	for _, want := range []string{
		`msg="rpc at https://base-mainnet.g.alchemy.com is slow"`,
		"rpc_label=https://base-mainnet.g.alchemy.com",
		"urls=\"[https://base-mainnet.g.alchemy.com postgres://db.internal:5432]\"",
		`Post \"https://base-mainnet.g.alchemy.com\": dial tcp: i/o timeout`,
		"nested.db=postgres://db.internal:5432",
		"nested.depth=2",
		"count=3",
		"preset=https://base-mainnet.g.alchemy.com",
		"g.error=\"connect postgres://db.internal:5432: refused\"",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log output lacks %q:\n%s", want, out)
		}
	}
}

func TestRedactingHandlerLeavesOtherValuesAlone(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewRedactingHandler(slog.NewTextHandler(&buf, nil), rpcURL))
	logger.Info("quote decision", "market", "cNGN-USDC", "price", 0.00073, "ok", true, "api", "https://api.numofx.com/v1/book?symbol=cNGN-USDC")
	out := buf.String()
	for _, want := range []string{"market=cNGN-USDC", "price=0.00073", "ok=true", "api=\"https://api.numofx.com/v1/book?symbol=cNGN-USDC\""} {
		if !strings.Contains(out, want) {
			t.Errorf("log output lacks %q:\n%s", want, out)
		}
	}
}
