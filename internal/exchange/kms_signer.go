package exchange

import (
	"bytes"
	"context"
	"crypto/x509/pkix"
	"encoding/asn1"
	"fmt"
	"math/big"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// KMSAPI is the two calls the signer makes, so tests can put a local key behind it.
type KMSAPI interface {
	GetPublicKey(ctx context.Context, params *kms.GetPublicKeyInput, optFns ...func(*kms.Options)) (*kms.GetPublicKeyOutput, error)
	Sign(ctx context.Context, params *kms.SignInput, optFns ...func(*kms.Options)) (*kms.SignOutput, error)
}

// kmsSignTimeout bounds one Sign round trip. The callers' contexts carry no deadline of their own,
// and a KMS call hanging on a half-open connection would otherwise stall the whole quoting cycle --
// including the cancels that keep a 60s rolling expiry from lapsing.
const kmsSignTimeout = 5 * time.Second

var (
	oidECPublicKey = asn1.ObjectIdentifier{1, 2, 840, 10045, 2, 1}
	oidSecp256k1   = asn1.ObjectIdentifier{1, 3, 132, 0, 10}

	secp256k1N     = crypto.S256().Params().N
	secp256k1HalfN = new(big.Int).Rsh(crypto.S256().Params().N, 1)
)

// KMSSigner signs with an AWS KMS ECC_SECG_P256K1 key. The private key never enters the process.
type KMSSigner struct {
	client  KMSAPI
	keyID   string
	address common.Address
	// pubkey is the uncompressed 65-byte point, kept so V can be found by recovery.
	pubkey []byte
}

// NewAWSKMSSigner builds a signer from the standard AWS credential and region chain (task role on
// Fargate, AWS_REGION from the environment).
func NewAWSKMSSigner(ctx context.Context, keyID string) (*KMSSigner, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	return NewKMSSigner(ctx, kms.NewFromConfig(awsCfg), keyID)
}

// NewKMSSigner fetches the key's public key once and caches the address derived from it.
//
// Fetched eagerly, not lazily on first sign: Address() has no way to report an error, the client
// needs the address before it places anything, and a key that is missing, the wrong spec, or not
// permitted to the task role should stop the process at boot rather than at the first quote.
func NewKMSSigner(ctx context.Context, client KMSAPI, keyID string) (*KMSSigner, error) {
	if keyID == "" {
		return nil, fmt.Errorf("kms key id is required")
	}
	out, err := client.GetPublicKey(ctx, &kms.GetPublicKeyInput{KeyId: aws.String(keyID)})
	if err != nil {
		return nil, fmt.Errorf("kms GetPublicKey %s: %w", keyID, err)
	}
	if out.KeySpec != "" && out.KeySpec != kmstypes.KeySpecEccSecgP256k1 {
		return nil, fmt.Errorf("kms key %s has spec %s, want %s", keyID, out.KeySpec, kmstypes.KeySpecEccSecgP256k1)
	}
	if out.KeyUsage != "" && out.KeyUsage != kmstypes.KeyUsageTypeSignVerify {
		return nil, fmt.Errorf("kms key %s has usage %s, want %s", keyID, out.KeyUsage, kmstypes.KeyUsageTypeSignVerify)
	}
	pubkey, err := secp256k1PointFromSPKI(out.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("kms key %s: %w", keyID, err)
	}
	parsed, err := crypto.UnmarshalPubkey(pubkey)
	if err != nil {
		return nil, fmt.Errorf("kms key %s: parse public key: %w", keyID, err)
	}
	return &KMSSigner{
		client:  client,
		keyID:   keyID,
		address: crypto.PubkeyToAddress(*parsed),
		pubkey:  pubkey,
	}, nil
}

func (s *KMSSigner) Address() common.Address { return s.address }

// SignHash asks KMS for an ECDSA signature over the digest and turns it into Ethereum's R||S||V.
//
// Two things KMS does not do for us:
//
//   - low-s. KMS returns whichever s the math produced, so about half its signatures are in the
//     upper half of the curve order. Ethereum (EIP-2) and OpenZeppelin's ECDSA.recover reject
//     those, so a raw KMS signature would fail on-chain verification half the time -- intermittently,
//     which is the worst way to fail. s > N/2 is replaced with N - s, which is the same signature.
//   - V. DER carries only (r, s). The recovery id is found by trying both and keeping the one that
//     recovers our own public key; if neither does, the signature is not ours and is refused.
func (s *KMSSigner) SignHash(ctx context.Context, hash []byte) ([]byte, error) {
	if len(hash) != 32 {
		return nil, fmt.Errorf("hash is required to be exactly 32 bytes (%d)", len(hash))
	}
	ctx, cancel := context.WithTimeout(ctx, kmsSignTimeout)
	defer cancel()
	out, err := s.client.Sign(ctx, &kms.SignInput{
		KeyId:            aws.String(s.keyID),
		Message:          hash,
		MessageType:      kmstypes.MessageTypeDigest,
		SigningAlgorithm: kmstypes.SigningAlgorithmSpecEcdsaSha256,
	})
	if err != nil {
		return nil, fmt.Errorf("kms Sign: %w", err)
	}
	return ethereumSignatureFromDER(hash, out.Signature, s.pubkey)
}

type ecdsaDERSignature struct {
	R, S *big.Int
}

// ethereumSignatureFromDER normalizes a DER (r, s) to low-s and appends the recovery id that makes
// it recover to pubkey.
func ethereumSignatureFromDER(hash, der, pubkey []byte) ([]byte, error) {
	var parsed ecdsaDERSignature
	rest, err := asn1.Unmarshal(der, &parsed)
	if err != nil {
		return nil, fmt.Errorf("parse kms signature: %w", err)
	}
	if len(rest) != 0 {
		return nil, fmt.Errorf("parse kms signature: %d trailing bytes", len(rest))
	}
	if parsed.R == nil || parsed.S == nil || parsed.R.Sign() <= 0 || parsed.S.Sign() <= 0 ||
		parsed.R.Cmp(secp256k1N) >= 0 || parsed.S.Cmp(secp256k1N) >= 0 {
		return nil, fmt.Errorf("kms signature (r, s) out of range")
	}
	sVal := new(big.Int).Set(parsed.S)
	if sVal.Cmp(secp256k1HalfN) > 0 {
		sVal.Sub(secp256k1N, sVal)
	}

	sig := make([]byte, 65)
	parsed.R.FillBytes(sig[0:32])
	sVal.FillBytes(sig[32:64])
	for v := byte(0); v <= 1; v++ {
		sig[64] = v
		recovered, err := crypto.Ecrecover(hash, sig)
		if err == nil && bytes.Equal(recovered, pubkey) {
			return sig, nil
		}
	}
	return nil, fmt.Errorf("kms signature does not recover to the key's public key")
}

type subjectPublicKeyInfo struct {
	Algorithm pkix.AlgorithmIdentifier
	PublicKey asn1.BitString
}

// secp256k1PointFromSPKI extracts the uncompressed point from the DER SubjectPublicKeyInfo that
// KMS GetPublicKey returns. crypto/x509 refuses secp256k1 as an unknown curve, so this is parsed by
// hand and the curve OID checked explicitly -- a P-256 key would otherwise parse into an address
// that no signature can ever recover to.
func secp256k1PointFromSPKI(der []byte) ([]byte, error) {
	var spki subjectPublicKeyInfo
	rest, err := asn1.Unmarshal(der, &spki)
	if err != nil {
		return nil, fmt.Errorf("parse SubjectPublicKeyInfo: %w", err)
	}
	if len(rest) != 0 {
		return nil, fmt.Errorf("parse SubjectPublicKeyInfo: %d trailing bytes", len(rest))
	}
	if !spki.Algorithm.Algorithm.Equal(oidECPublicKey) {
		return nil, fmt.Errorf("public key algorithm %v is not ecPublicKey", spki.Algorithm.Algorithm)
	}
	var curve asn1.ObjectIdentifier
	if _, err := asn1.Unmarshal(spki.Algorithm.Parameters.FullBytes, &curve); err != nil {
		return nil, fmt.Errorf("parse curve parameters: %w", err)
	}
	if !curve.Equal(oidSecp256k1) {
		return nil, fmt.Errorf("curve %v is not secp256k1", curve)
	}
	point := spki.PublicKey.RightAlign()
	if len(point) != 65 || point[0] != 0x04 {
		return nil, fmt.Errorf("public key is not an uncompressed secp256k1 point (%d bytes)", len(point))
	}
	return point, nil
}
