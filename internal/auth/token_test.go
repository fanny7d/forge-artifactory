package auth

import (
	"bytes"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestIssueTokenProducesParseableBearerAndHMAC(t *testing.T) {
	tokenID := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	pepper := bytes.Repeat([]byte{0x41}, 32)
	random := bytes.NewReader(bytes.Repeat([]byte{0x7f}, 32))

	issued, err := IssueToken(random, tokenID, pepper)
	if err != nil {
		t.Fatalf("IssueToken() error = %v", err)
	}
	wantSecret := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x7f}, 32))
	wantBearer := "ar1." + tokenID.String() + "." + wantSecret
	if issued.Bearer != wantBearer {
		t.Fatalf("Bearer = %q, want %q", issued.Bearer, wantBearer)
	}
	if len(issued.SecretHMAC) != 32 {
		t.Fatalf("SecretHMAC length = %d, want 32", len(issued.SecretHMAC))
	}

	parsedID, secret, err := ParseBearer(issued.Bearer)
	if err != nil {
		t.Fatalf("ParseBearer() error = %v", err)
	}
	if parsedID != tokenID {
		t.Fatalf("token ID = %s, want %s", parsedID, tokenID)
	}
	if !VerifySecret(pepper, parsedID, secret, issued.SecretHMAC) {
		t.Fatal("VerifySecret() = false, want true")
	}
}

func TestParseBearerRejectsMalformedValues(t *testing.T) {
	for _, bearer := range []string{
		"",
		"ar2.11111111-2222-4333-8444-555555555555.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"ar1.not-a-uuid.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"ar1.11111111-2222-4333-8444-555555555555.short",
	} {
		t.Run(bearer, func(t *testing.T) {
			if _, _, err := ParseBearer(bearer); !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("ParseBearer(%q) error = %v, want ErrInvalidToken", bearer, err)
			}
		})
	}
}
