package idempotency

import (
	"bytes"
	"testing"
)

func TestSealerRoundTripsAndAuthenticatesAAD(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	nonce := bytes.NewReader(bytes.Repeat([]byte{0x24}, 12))
	sealer, err := NewSealer(key, nonce)
	if err != nil {
		t.Fatalf("NewSealer() error = %v", err)
	}

	plaintext := []byte(`{"secret":"ar1.sensitive"}`)
	aad := []byte("token|POST|/api/v1/service-accounts/id/tokens|request-key")
	sealed, err := sealer.Seal(plaintext, aad)
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	if bytes.Contains(sealed, []byte("sensitive")) {
		t.Fatal("sealed response contains plaintext")
	}
	opened, err := sealer.Open(sealed, aad)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if !bytes.Equal(opened, plaintext) {
		t.Fatalf("Open() = %q, want %q", opened, plaintext)
	}
	if _, err := sealer.Open(sealed, []byte("different")); err == nil {
		t.Fatal("Open() with different AAD succeeded")
	}
}

func TestNewSealerRequiresAES256Key(t *testing.T) {
	if _, err := NewSealer(make([]byte, 16), bytes.NewReader(make([]byte, 12))); err == nil {
		t.Fatal("NewSealer() with 16-byte key succeeded")
	}
}
