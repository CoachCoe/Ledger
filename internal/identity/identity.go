// Package identity holds Ledger's signing keys: the human root key, kept
// outside every repository, and ephemeral in-memory session keys (ADR 0008).
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// Signer signs record envelopes. ID is the envelope signer value: a root
// key fingerprint or a session id.
type Signer interface {
	ID() string
	PublicKey() ed25519.PublicKey
	Sign(message []byte) []byte
}

// IdentityProvider supplies the human root signer. Local key files are the
// only Phase 1 implementation; SSO/OIDC providers can satisfy it later.
type IdentityProvider interface {
	Root() (Signer, error)
}

type keySigner struct {
	id   string
	priv ed25519.PrivateKey
}

func (k *keySigner) ID() string                   { return k.id }
func (k *keySigner) PublicKey() ed25519.PublicKey { return k.priv.Public().(ed25519.PublicKey) }
func (k *keySigner) Sign(m []byte) []byte         { return ed25519.Sign(k.priv, m) }

// RootSigner returns a signer identified by the fingerprint of priv's
// public key.
func RootSigner(priv ed25519.PrivateKey) Signer {
	return &keySigner{id: Fingerprint(priv.Public().(ed25519.PublicKey)), priv: priv}
}

// NewSession generates an ephemeral key for sessionID. The private key
// exists only in the returned value.
func NewSession(sessionID string) (Signer, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &keySigner{id: sessionID, priv: priv}, nil
}

// SessionSigner wraps an existing private key as the signer for sessionID.
func SessionSigner(sessionID string, priv ed25519.PrivateKey) Signer {
	return &keySigner{id: sessionID, priv: priv}
}

// Fingerprint is the lowercase hex SHA-256 of the raw 32-byte public key.
func Fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])
}

var b64 = base64.RawURLEncoding

// EncodePublicKey returns pub as unpadded base64url.
func EncodePublicKey(pub ed25519.PublicKey) string { return b64.EncodeToString(pub) }

// DecodePublicKey parses an unpadded base64url Ed25519 public key.
func DecodePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := b64.Strict().DecodeString(s)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("identity: malformed public key")
	}
	return ed25519.PublicKey(b), nil
}

// EncodeSignature returns sig as unpadded base64url.
func EncodeSignature(sig []byte) string { return b64.EncodeToString(sig) }

// VerifySignature reports whether sig (unpadded base64url) is pub's
// signature of message.
func VerifySignature(pub ed25519.PublicKey, message []byte, sig string) bool {
	b, err := b64.Strict().DecodeString(sig)
	return err == nil && len(pub) == ed25519.PublicKeySize && len(b) == ed25519.SignatureSize && ed25519.Verify(pub, message, b)
}
