package bootstrap

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type KeyInfo struct {
	KeyID       string
	Fingerprint string
}

func GenerateKeyPair(privatePath, publicPath string, random io.Reader) (KeyInfo, error) {
	if privatePath == "" || publicPath == "" || privatePath == publicPath {
		return KeyInfo{}, fmt.Errorf("private and public key paths must be distinct")
	}
	if random == nil {
		return KeyInfo{}, fmt.Errorf("key generation random reader is nil")
	}
	if err := os.MkdirAll(filepath.Dir(privatePath), 0o755); err != nil {
		return KeyInfo{}, fmt.Errorf("create private key directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(publicPath), 0o755); err != nil {
		return KeyInfo{}, fmt.Errorf("create public key directory: %w", err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(random)
	if err != nil {
		return KeyInfo{}, fmt.Errorf("generate Ed25519 key: %w", err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return KeyInfo{}, fmt.Errorf("marshal Ed25519 private key: %w", err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return KeyInfo{}, fmt.Errorf("marshal Ed25519 public key: %w", err)
	}
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
	if err := writeExclusive(privatePath, privatePEM, 0o600); err != nil {
		return KeyInfo{}, fmt.Errorf("write Ed25519 private key: %w", err)
	}
	if err := writeExclusive(publicPath, publicPEM, 0o644); err != nil {
		_ = os.Remove(privatePath)
		return KeyInfo{}, fmt.Errorf("write Ed25519 public key: %w", err)
	}
	fingerprint := sha256.Sum256(publicKey)
	fingerprintHex := hex.EncodeToString(fingerprint[:])
	return KeyInfo{KeyID: "ed25519:" + fingerprintHex, Fingerprint: fingerprintHex}, nil
}

func writeExclusive(path string, content []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(content); err != nil {
		return err
	}
	if err := file.Chmod(mode); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	remove = false
	return nil
}
