package signing

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
)

var ErrInvalidKey = errors.New("invalid Ed25519 signing key")

type Ed25519 struct {
	privateKey ed25519.PrivateKey
	publicKey  ed25519.PublicKey
	keyID      string
}

func NewEd25519(privateKey ed25519.PrivateKey) (*Ed25519, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("%w: private key must be %d bytes", ErrInvalidKey, ed25519.PrivateKeySize)
	}
	privateCopy := append(ed25519.PrivateKey(nil), privateKey...)
	publicKey, ok := privateCopy.Public().(ed25519.PublicKey)
	if !ok || len(publicKey) != ed25519.PublicKeySize {
		return nil, ErrInvalidKey
	}
	publicCopy := append(ed25519.PublicKey(nil), publicKey...)
	fingerprint := sha256.Sum256(publicCopy)
	return &Ed25519{
		privateKey: privateCopy,
		publicKey:  publicCopy,
		keyID:      "ed25519:" + hex.EncodeToString(fingerprint[:]),
	}, nil
}

func LoadEd25519(privateKeyPath, publicKeyPath string) (*Ed25519, error) {
	info, err := os.Stat(privateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("stat Ed25519 private key: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%w: private key must be a regular file with mode 0600 or stricter", ErrInvalidKey)
	}
	privatePEM, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read Ed25519 private key: %w", err)
	}
	privateBlock, trailing := pem.Decode(privatePEM)
	if privateBlock == nil || len(bytes.TrimSpace(trailing)) != 0 {
		return nil, fmt.Errorf("%w: private key is not one PEM block", ErrInvalidKey)
	}
	parsedPrivate, err := x509.ParsePKCS8PrivateKey(privateBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: parse PKCS#8 private key: %v", ErrInvalidKey, err)
	}
	privateKey, ok := parsedPrivate.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%w: private key is not Ed25519", ErrInvalidKey)
	}
	signer, err := NewEd25519(privateKey)
	if err != nil {
		return nil, err
	}

	publicPEM, err := os.ReadFile(publicKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read Ed25519 public key: %w", err)
	}
	publicBlock, trailing := pem.Decode(publicPEM)
	if publicBlock == nil || len(bytes.TrimSpace(trailing)) != 0 {
		return nil, fmt.Errorf("%w: public key is not one PEM block", ErrInvalidKey)
	}
	parsedPublic, err := x509.ParsePKIXPublicKey(publicBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: parse PKIX public key: %v", ErrInvalidKey, err)
	}
	publicKey, ok := parsedPublic.(ed25519.PublicKey)
	if !ok || !bytes.Equal(publicKey, signer.publicKey) {
		return nil, fmt.Errorf("%w: public key does not match private key", ErrInvalidKey)
	}
	return signer, nil
}

func (s *Ed25519) KeyID() string {
	return s.keyID
}

func (s *Ed25519) Sign(ctx context.Context, message []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	signature := ed25519.Sign(s.privateKey, message)
	return append([]byte(nil), signature...), nil
}

func (s *Ed25519) PublicKey() ed25519.PublicKey {
	return append(ed25519.PublicKey(nil), s.publicKey...)
}
