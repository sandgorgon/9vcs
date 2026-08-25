// Package identity implements per-install peer identity for 9vcs: a
// long-lived Ed25519 keypair wrapped in a minimal self-signed X.509
// certificate (Go's crypto/tls API requires a certificate even for a bare
// keypair), a fingerprint derived from the public key for out-of-band
// exchange, and TLS 1.3 config construction that authenticates peers by
// exact fingerprint match instead of a CA chain — there is no CA in this
// design, see PLAN.md "Auth: pinned-fingerprint TLS, not a CA".
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// Identity is this install's long-lived peer identity.
type Identity struct {
	Key ed25519.PrivateKey
	Raw []byte // DER-encoded self-signed certificate
}

// Fingerprint is a stable identifier for a public key: SHA-256 of the raw
// Ed25519 public key bytes, hex-encoded — the same computation Identity's
// own fingerprint and a peer's presented certificate both reduce to, so
// they're directly comparable.
func Fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])
}

// Fingerprint returns this identity's own fingerprint, for `9vcs identity
// show`'s out-of-band exchange.
func (id *Identity) Fingerprint() string {
	return Fingerprint(id.Key.Public().(ed25519.PublicKey))
}

// FingerprintOf returns the fingerprint of a parsed peer certificate's
// public key. Fails on any key type other than Ed25519 — this design only
// ever issues Ed25519 identities, so a peer presenting anything else isn't
// a 9vcs peer at all.
func FingerprintOf(cert *x509.Certificate) (string, error) {
	pub, ok := cert.PublicKey.(ed25519.PublicKey)
	if !ok {
		return "", fmt.Errorf("identity: peer certificate has unsupported key type %T", cert.PublicKey)
	}
	return Fingerprint(pub), nil
}

// ConfigDir is where this install's identity lives — machine-wide, not
// per-repo: every 9vcs repo on this machine shares one identity, matching
// PLAN.md ("each 9vcs install generates a long-lived Ed25519 keypair").
// Exported so other 9vcs-owned files that belong in this same directory
// (e.g. cmd/9vcs's user config) don't need to re-derive the path.
func ConfigDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "9vcs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// Load loads this install's identity, generating and persisting a new one
// on first use.
func Load() (*Identity, error) {
	dir, err := ConfigDir()
	if err != nil {
		return nil, err
	}
	return loadFrom(filepath.Join(dir, "identity.key"), filepath.Join(dir, "identity.cert"))
}

// loadFrom is Load's file logic against explicit paths, split out so tests
// can exercise it without touching the real user config directory.
func loadFrom(keyPath, certPath string) (*Identity, error) {
	if _, err := os.Stat(keyPath); errors.Is(err, os.ErrNotExist) {
		id, err := generate()
		if err != nil {
			return nil, err
		}
		if err := id.save(keyPath, certPath); err != nil {
			return nil, err
		}
		return id, nil
	}

	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("identity: reading %s: %w", keyPath, err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, fmt.Errorf("identity: %s is not valid PEM", keyPath)
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("identity: parsing %s: %w", keyPath, err)
	}
	key, ok := keyAny.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("identity: %s holds a %T key, not Ed25519", keyPath, keyAny)
	}

	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("identity: reading %s: %w", certPath, err)
	}
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, fmt.Errorf("identity: %s is not valid PEM", certPath)
	}

	return &Identity{Key: key, Raw: certBlock.Bytes}, nil
}

func generate() (*Identity, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "9vcs"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(100, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return nil, fmt.Errorf("identity: creating self-signed certificate: %w", err)
	}
	return &Identity{Key: priv, Raw: der}, nil
}

func (id *Identity) save(keyPath, certPath string) error {
	keyDER, err := x509.MarshalPKCS8PrivateKey(id.Key)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: id.Raw})
	return os.WriteFile(certPath, certPEM, 0o644)
}
