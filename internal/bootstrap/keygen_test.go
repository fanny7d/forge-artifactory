package bootstrap

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/signing"
)

func TestGenerateKeyPairWritesSignerCompatiblePEMFiles(t *testing.T) {
	directory := t.TempDir()
	privatePath := filepath.Join(directory, "private.pem")
	publicPath := filepath.Join(directory, "public.pem")
	seed := bytes.Repeat([]byte{0x91}, 32)
	info, err := GenerateKeyPair(privatePath, publicPath, bytes.NewReader(seed))
	if err != nil {
		t.Fatalf("GenerateKeyPair() error = %v", err)
	}
	privateInfo, err := os.Stat(privatePath)
	if err != nil {
		t.Fatalf("stat private key: %v", err)
	}
	publicInfo, err := os.Stat(publicPath)
	if err != nil {
		t.Fatalf("stat public key: %v", err)
	}
	if privateInfo.Mode().Perm() != 0o600 || publicInfo.Mode().Perm() != 0o644 {
		t.Fatalf("key modes = private %o public %o", privateInfo.Mode().Perm(), publicInfo.Mode().Perm())
	}
	signer, err := signing.LoadEd25519(privatePath, publicPath)
	if err != nil {
		t.Fatalf("LoadEd25519() error = %v", err)
	}
	fingerprint := sha256.Sum256(signer.PublicKey())
	if info.KeyID != signer.KeyID() || info.Fingerprint != fingerprintHex(fingerprint[:]) {
		t.Fatalf("key info = %+v signer key ID = %q", info, signer.KeyID())
	}
}

func fingerprintHex(value []byte) string {
	const hex = "0123456789abcdef"
	result := make([]byte, len(value)*2)
	for index, item := range value {
		result[index*2] = hex[item>>4]
		result[index*2+1] = hex[item&0x0f]
	}
	return string(result)
}
