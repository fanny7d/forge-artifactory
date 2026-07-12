package signing

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEd25519SignerUsesPublicKeyFingerprintAndVerifiableSignature(t *testing.T) {
	seed := sha256.Sum256([]byte("artifact-repository-test-key"))
	privateKey := ed25519.NewKeyFromSeed(seed[:])
	signer, err := NewEd25519(privateKey)
	if err != nil {
		t.Fatalf("NewEd25519() error = %v", err)
	}
	message := []byte(`{"schemaVersion":1}`)
	signature, err := signer.Sign(t.Context(), message)
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	publicKey := signer.PublicKey()
	if !ed25519.Verify(publicKey, message, signature) {
		t.Fatal("signature did not verify")
	}
	fingerprint := sha256.Sum256(publicKey)
	if signer.KeyID() != "ed25519:"+hex.EncodeToString(fingerprint[:]) {
		t.Fatalf("KeyID() = %q", signer.KeyID())
	}
	if !strings.HasPrefix(signer.KeyID(), "ed25519:") {
		t.Fatalf("KeyID() = %q", signer.KeyID())
	}
	publicKey[0] ^= 0xff
	if signer.PublicKey()[0] == publicKey[0] {
		t.Fatal("PublicKey() exposed mutable signer state")
	}
}

func TestLoadEd25519AcceptsPKCS8AndPKIXPEM(t *testing.T) {
	privatePath, publicPath, privateKey := writeEd25519KeyPair(t, "matching-key")
	signer, err := LoadEd25519(privatePath, publicPath)
	if err != nil {
		t.Fatalf("LoadEd25519() error = %v", err)
	}
	message := []byte("signed manifest")
	signature, err := signer.Sign(t.Context(), message)
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	if !ed25519.Verify(privateKey.Public().(ed25519.PublicKey), message, signature) {
		t.Fatal("loaded signer produced an invalid signature")
	}
}

func TestLoadEd25519RejectsPermissivePrivateKeyAndMismatchedPublicKey(t *testing.T) {
	t.Run("permissions", func(t *testing.T) {
		privatePath, publicPath, _ := writeEd25519KeyPair(t, "permissive-key")
		if err := os.Chmod(privatePath, 0o640); err != nil {
			t.Fatalf("chmod private key: %v", err)
		}
		if _, err := LoadEd25519(privatePath, publicPath); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("LoadEd25519() error = %v, want ErrInvalidKey", err)
		}
	})

	t.Run("mismatched public key", func(t *testing.T) {
		privatePath, _, _ := writeEd25519KeyPair(t, "private-key")
		_, publicPath, _ := writeEd25519KeyPair(t, "different-public-key")
		if _, err := LoadEd25519(privatePath, publicPath); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("LoadEd25519() error = %v, want ErrInvalidKey", err)
		}
	})
}

func writeEd25519KeyPair(t *testing.T, label string) (string, string, ed25519.PrivateKey) {
	t.Helper()
	seed := sha256.Sum256([]byte(label))
	privateKey := ed25519.NewKeyFromSeed(seed[:])
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(privateKey.Public())
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	directory := t.TempDir()
	privatePath := filepath.Join(directory, "signing-private.pem")
	publicPath := filepath.Join(directory, "signing-public.pem")
	if err := os.WriteFile(privatePath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), 0o600); err != nil {
		t.Fatalf("write private key: %v", err)
	}
	if err := os.Chmod(privatePath, 0o600); err != nil {
		t.Fatalf("chmod private key: %v", err)
	}
	if err := os.WriteFile(publicPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}), 0o644); err != nil {
		t.Fatalf("write public key: %v", err)
	}
	return privatePath, publicPath, privateKey
}
