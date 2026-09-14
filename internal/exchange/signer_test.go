package exchange

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/x509/pkix"
	"encoding/asn1"
	"math/big"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
)

// fakeKMS stands a local secp256k1 key behind the KMS API, answering in the same encodings KMS
// uses: a DER SubjectPublicKeyInfo from GetPublicKey, and a DER (r, s) from Sign with no recovery id.
type fakeKMS struct {
	key *ecdsa.PrivateKey
	// forceHighS returns N - s, which is what KMS hands back for roughly half of all signatures.
	forceHighS bool
	// signWith, when set, signs with a different key than the one GetPublicKey reports.
	signWith *ecdsa.PrivateKey
	lastS    *big.Int
}

func (f *fakeKMS) GetPublicKey(_ context.Context, _ *kms.GetPublicKeyInput, _ ...func(*kms.Options)) (*kms.GetPublicKeyOutput, error) {
	params, err := asn1.Marshal(oidSecp256k1)
	if err != nil {
		return nil, err
	}
	point := crypto.FromECDSAPub(&f.key.PublicKey)
	der, err := asn1.Marshal(subjectPublicKeyInfo{
		Algorithm: pkix.AlgorithmIdentifier{Algorithm: oidECPublicKey, Parameters: asn1.RawValue{FullBytes: params}},
		PublicKey: asn1.BitString{Bytes: point, BitLength: len(point) * 8},
	})
	if err != nil {
		return nil, err
	}
	return &kms.GetPublicKeyOutput{
		PublicKey: der,
		KeySpec:   kmstypes.KeySpecEccSecgP256k1,
		KeyUsage:  kmstypes.KeyUsageTypeSignVerify,
	}, nil
}

func (f *fakeKMS) Sign(_ context.Context, in *kms.SignInput, _ ...func(*kms.Options)) (*kms.SignOutput, error) {
	key := f.key
	if f.signWith != nil {
		key = f.signWith
	}
	sig, err := crypto.Sign(in.Message, key)
	if err != nil {
		return nil, err
	}
	r := new(big.Int).SetBytes(sig[0:32])
	s := new(big.Int).SetBytes(sig[32:64])
	if f.forceHighS {
		s = new(big.Int).Sub(secp256k1N, s)
	}
	f.lastS = s
	der, err := asn1.Marshal(ecdsaDERSignature{R: r, S: s})
	if err != nil {
		return nil, err
	}
	return &kms.SignOutput{Signature: der}, nil
}

func testKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := crypto.HexToECDSA(testSignerKey)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	return key
}

func TestKMSSignerDerivesTheAddressFromTheSPKI(t *testing.T) {
	key := testKey(t)
	signer, err := NewKMSSigner(context.Background(), &fakeKMS{key: key}, "alias/mm")
	if err != nil {
		t.Fatalf("NewKMSSigner: %v", err)
	}
	if want := crypto.PubkeyToAddress(key.PublicKey); signer.Address() != want {
		t.Fatalf("address %s, want %s", signer.Address().Hex(), want.Hex())
	}
}

// Roughly half of real KMS signatures have s in the upper half of the order. Ethereum rejects
// those, so without normalization the bot would fail verification intermittently -- and the fake
// only proves anything if it really did hand back a high s.
func TestKMSSignerNormalizesHighSAndRecoversTheAddress(t *testing.T) {
	key := testKey(t)
	fake := &fakeKMS{key: key, forceHighS: true}
	signer, err := NewKMSSigner(context.Background(), fake, "alias/mm")
	if err != nil {
		t.Fatalf("NewKMSSigner: %v", err)
	}
	hash := crypto.Keccak256([]byte("numo"))

	sig, err := signer.SignHash(context.Background(), hash)
	if err != nil {
		t.Fatalf("SignHash: %v", err)
	}
	if fake.lastS.Cmp(secp256k1HalfN) <= 0 {
		t.Fatal("the fake did not produce a high s, so this test proves nothing")
	}
	if s := new(big.Int).SetBytes(sig[32:64]); s.Cmp(secp256k1HalfN) > 0 {
		t.Fatalf("s was not normalized to the lower half: %s", s)
	}
	if sig[64] > 1 {
		t.Fatalf("v = %d, want 0 or 1 so call sites keep adding 27", sig[64])
	}
	pub, err := crypto.SigToPub(hash, sig)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if got := crypto.PubkeyToAddress(*pub); got != signer.Address() {
		t.Fatalf("recovered %s, want %s", got.Hex(), signer.Address().Hex())
	}
}

// A signature that recovers to neither parity is not from our key. It is refused rather than
// shipped with a guessed V, which the venue would attribute to a stranger.
func TestKMSSignerRefusesASignatureFromAnotherKey(t *testing.T) {
	other, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	signer, err := NewKMSSigner(context.Background(), &fakeKMS{key: testKey(t), signWith: other}, "alias/mm")
	if err != nil {
		t.Fatalf("NewKMSSigner: %v", err)
	}
	if _, err := signer.SignHash(context.Background(), crypto.Keccak256([]byte("numo"))); err == nil {
		t.Fatal("a signature from a different key must not be accepted")
	}
}

// Both signers must produce byte-identical EIP-712 signatures for the same key. That holds here
// because go-ethereum's crypto.Sign is deterministic (RFC6979) and the fake derives its DER from
// it -- after low-s normalization the (r, s) pair is exactly the local one, and V is recovered, not
// copied. Real KMS uses a random nonce, so in production the two differ byte-for-byte while both
// recover to the same address; the recovery assertions above are what cover that case.
func TestKMSSignerMatchesTheLocalSignerForActionsAndCancels(t *testing.T) {
	key := testKey(t)
	addr := crypto.PubkeyToAddress(key.PublicKey)
	kmsSigner, err := NewKMSSigner(context.Background(), &fakeKMS{key: key, forceHighS: true}, "alias/mm")
	if err != nil {
		t.Fatalf("NewKMSSigner: %v", err)
	}
	local := &HTTPClient{signer: NewLocalSigner(key), matching: common.HexToAddress(testMatchingAddress), cfg: ClientConfig{ChainID: 8453}}
	remote := &HTTPClient{signer: kmsSigner, matching: common.HexToAddress(testMatchingAddress), cfg: ClientConfig{ChainID: 8453}}

	action := map[string]string{
		"subaccount_id": "10",
		"nonce":         "3574117736041901",
		"module":        "0x1111111111111111111111111111111111111111",
		"data":          hexutil.Encode([]byte{0xde, 0xad, 0xbe, 0xef}),
		"expiry":        "1787058143",
		"owner":         addr.Hex(),
		"signer":        addr.Hex(),
	}
	localAction, err := local.signAction(context.Background(), action)
	if err != nil {
		t.Fatalf("local signAction: %v", err)
	}
	kmsAction, err := remote.signAction(context.Background(), action)
	if err != nil {
		t.Fatalf("kms signAction: %v", err)
	}
	if localAction != kmsAction {
		t.Fatalf("action signatures differ:\n local %s\n kms   %s", localAction, kmsAction)
	}

	localCancel, err := local.signCancel(context.Background(), addr.Hex(), addr.Hex(), "3574117736041901", "1787058143")
	if err != nil {
		t.Fatalf("local signCancel: %v", err)
	}
	kmsCancel, err := remote.signCancel(context.Background(), addr.Hex(), addr.Hex(), "3574117736041901", "1787058143")
	if err != nil {
		t.Fatalf("kms signCancel: %v", err)
	}
	if localCancel != kmsCancel {
		t.Fatalf("cancel signatures differ:\n local %s\n kms   %s", localCancel, kmsCancel)
	}
	if got := recoverCancelSigner(t, remote.matching, addr.Hex(), addr.Hex(), "3574117736041901", "1787058143", kmsCancel); got != addr {
		t.Fatalf("kms cancel recovered %s, want %s", got.Hex(), addr.Hex())
	}
}

// A KMS key that is not the configured owner would sign orders for someone else's subaccount, and
// one that is not the configured signer would make every cancel fail verification. Both stop boot.
func TestResolveSignerRefusesAnAddressMismatch(t *testing.T) {
	signer, err := NewKMSSigner(context.Background(), &fakeKMS{key: testKey(t)}, "alias/mm")
	if err != nil {
		t.Fatalf("NewKMSSigner: %v", err)
	}
	stranger := "0x000000000000000000000000000000000000dEaD"

	if _, _, err := resolveSigner(ClientConfig{Signer: signer, OwnerAddress: stranger}); err == nil || !strings.Contains(err.Error(), "MM_OWNER_ADDRESS") {
		t.Fatalf("owner mismatch: err = %v", err)
	}
	if _, _, err := resolveSigner(ClientConfig{Signer: signer, SignerAddress: stranger}); err == nil || !strings.Contains(err.Error(), "MM_SIGNER_ADDRESS") {
		t.Fatalf("signer mismatch: err = %v", err)
	}

	// A checksummed or lower-cased configured address is the same address, and resolves with no
	// private key configured at all.
	_, cfg, err := resolveSigner(ClientConfig{
		Signer:        signer,
		OwnerAddress:  signer.Address().Hex(),
		SignerAddress: strings.ToLower(signer.Address().Hex()),
	})
	if err != nil {
		t.Fatalf("matching owner and signer: %v", err)
	}
	want := strings.ToLower(signer.Address().Hex())
	if cfg.OwnerAddress != want || cfg.SignerAddress != want {
		t.Fatalf("addresses = %s / %s, want both %s", cfg.OwnerAddress, cfg.SignerAddress, want)
	}
}

// The local path is unchanged: same signature crypto.Sign would have produced.
func TestLocalSignerIsCryptoSign(t *testing.T) {
	key := testKey(t)
	hash := crypto.Keccak256([]byte("numo"))
	want, err := crypto.Sign(hash, key)
	if err != nil {
		t.Fatalf("crypto.Sign: %v", err)
	}
	got, err := NewLocalSigner(key).SignHash(context.Background(), hash)
	if err != nil {
		t.Fatalf("SignHash: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("local signer diverged from crypto.Sign")
	}
}
