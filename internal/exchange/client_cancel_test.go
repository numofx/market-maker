package exchange

import (
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	gethmath "github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
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

// The Matching contract on Base mainnet, matching the production domain.
const testMatchingAddress = "0x9E90A9cD13d859Bd6a08168082FB1F6F7405F191"

// A fixed key so the test is deterministic; its address is derived, never hardcoded.
const testSignerKey = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"

// signCancel must produce a signature markets-service can verify: it has to recover to the signer
// over the Cancel digest, and every field must be bound so a captured signature cannot be replayed
// against a different order. This mirrors the server's own cancel-signature check.
func TestSignCancelRecoversSigner(t *testing.T) {
	key, err := crypto.HexToECDSA(testSignerKey)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	addr := crypto.PubkeyToAddress(key.PublicKey)
	c := &HTTPClient{
		signerKey: key,
		matching:  common.HexToAddress(testMatchingAddress),
		cfg:       ClientConfig{ChainID: 8453},
	}
	nonce := "3574117736041901"
	expiry := "1787058143"

	sigHex, err := c.signCancel(addr.Hex(), addr.Hex(), nonce, expiry)
	if err != nil {
		t.Fatalf("signCancel: %v", err)
	}

	if got := recoverCancelSigner(t, c.matching, addr.Hex(), addr.Hex(), nonce, expiry, sigHex); got != addr {
		t.Fatalf("recovered %s, want %s", got.Hex(), addr.Hex())
	}

	// The nonce is the cancel key: a signature over one nonce must not authorize cancelling another.
	if got := recoverCancelSigner(t, c.matching, addr.Hex(), addr.Hex(), "3574117736041902", expiry, sigHex); got == addr {
		t.Fatal("a signature over a different nonce must not recover the signer")
	}
}

// recoverCancelSigner rebuilds the Cancel digest and returns the address that produced sigHex.
func recoverCancelSigner(t *testing.T, matching common.Address, owner, signer, nonce, expiry, sigHex string) common.Address {
	t.Helper()
	td := apitypes.TypedData{
		Types: apitypes.Types{
			"EIP712Domain": {
				{Name: "name", Type: "string"},
				{Name: "version", Type: "string"},
				{Name: "chainId", Type: "uint256"},
				{Name: "verifyingContract", Type: "address"},
			},
			"Cancel": {
				{Name: "owner", Type: "address"},
				{Name: "signer", Type: "address"},
				{Name: "nonce", Type: "uint256"},
				{Name: "expiry", Type: "uint256"},
			},
		},
		PrimaryType: "Cancel",
		Domain: apitypes.TypedDataDomain{
			Name:              "Matching",
			Version:           "1.0",
			ChainId:           (*gethmath.HexOrDecimal256)(big.NewInt(8453)),
			VerifyingContract: matching.Hex(),
		},
		Message: apitypes.TypedDataMessage{
			"owner":  owner,
			"signer": signer,
			"nonce":  nonce,
			"expiry": expiry,
		},
	}
	hash, _, err := apitypes.TypedDataAndHash(td)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	sig := hexutil.MustDecode(sigHex)
	sig[64] -= 27
	pub, err := crypto.SigToPub(hash, sig)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	return crypto.PubkeyToAddress(*pub)
}
