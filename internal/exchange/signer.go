package exchange

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Signer produces secp256k1 signatures over a 32-byte digest for one Ethereum address.
//
// SignHash returns 65 bytes R||S||V with V in {0,1} -- exactly what crypto.Sign returns -- so the
// EIP-712 call sites keep adding 27 themselves and nothing about the wire format moves when the
// key does.
//
// It exists so the key can leave the process. A raw *ecdsa.PrivateKey in an env var is readable by
// anything that can read the task definition or the process's memory, and a market maker's key
// controls every resting order and the subaccount behind them. KMS keeps the key material out of
// the task entirely; this interface is the seam that lets signAction and signCancel not care.
type Signer interface {
	Address() common.Address
	SignHash(ctx context.Context, hash []byte) ([]byte, error)
}

// localSigner is the historical behaviour: an in-process key, signed with crypto.Sign.
type localSigner struct {
	key     *ecdsa.PrivateKey
	address common.Address
}

// NewLocalSigner wraps an in-process key. Behaviour is identical to the crypto.Sign calls it
// replaced, including the deterministic RFC6979 nonce.
func NewLocalSigner(key *ecdsa.PrivateKey) Signer {
	return &localSigner{key: key, address: crypto.PubkeyToAddress(key.PublicKey)}
}

func (s *localSigner) Address() common.Address { return s.address }

func (s *localSigner) SignHash(_ context.Context, hash []byte) ([]byte, error) {
	return crypto.Sign(hash, s.key)
}

// resolveSigner decides which key signs and what the owner/signer addresses are, before anything
// touches the network.
//
// An injected Signer (KMS) is the only key: the addresses default to its address and, when they
// are configured, must equal it. That check is a hard failure rather than a warning because the two
// ways it can go wrong are both silent until they hurt: an owner mismatch signs orders the venue
// attributes to someone else's subaccount, and a signer mismatch makes every cancel fail
// verification -- markets-service requires signer == owner for cancels -- which on a 60s rolling
// expiry means the bot cannot take down a single quote.
//
// Without an injected Signer the private-key path is unchanged: the owner key supplies the default
// owner address and the signer key (defaulting to the owner key) signs.
func resolveSigner(cfg ClientConfig) (Signer, ClientConfig, error) {
	if cfg.Signer != nil {
		derived := strings.ToLower(cfg.Signer.Address().Hex())
		if cfg.OwnerAddress != "" && !strings.EqualFold(strings.TrimSpace(cfg.OwnerAddress), derived) {
			return nil, cfg, fmt.Errorf("signer address %s does not match MM_OWNER_ADDRESS %s", derived, cfg.OwnerAddress)
		}
		if cfg.SignerAddress != "" && !strings.EqualFold(strings.TrimSpace(cfg.SignerAddress), derived) {
			return nil, cfg, fmt.Errorf("signer address %s does not match MM_SIGNER_ADDRESS %s (cancels require signer == owner)", derived, cfg.SignerAddress)
		}
		cfg.OwnerAddress = derived
		cfg.SignerAddress = derived
		return cfg.Signer, cfg, nil
	}

	if cfg.OwnerPrivateKey == "" {
		return nil, cfg, fmt.Errorf("OwnerPrivateKey is required")
	}
	if cfg.SignerPrivateKey == "" {
		cfg.SignerPrivateKey = cfg.OwnerPrivateKey
	}
	ownerKey, err := crypto.HexToECDSA(strings.TrimPrefix(cfg.OwnerPrivateKey, "0x"))
	if err != nil {
		return nil, cfg, fmt.Errorf("parse owner private key: %w", err)
	}
	signerKey, err := crypto.HexToECDSA(strings.TrimPrefix(cfg.SignerPrivateKey, "0x"))
	if err != nil {
		return nil, cfg, fmt.Errorf("parse signer private key: %w", err)
	}
	if cfg.OwnerAddress == "" {
		cfg.OwnerAddress = crypto.PubkeyToAddress(ownerKey.PublicKey).Hex()
	}
	if cfg.SignerAddress == "" {
		cfg.SignerAddress = crypto.PubkeyToAddress(signerKey.PublicKey).Hex()
	}
	cfg.OwnerAddress = strings.ToLower(cfg.OwnerAddress)
	cfg.SignerAddress = strings.ToLower(cfg.SignerAddress)
	return NewLocalSigner(signerKey), cfg, nil
}
