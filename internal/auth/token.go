package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"
)

const (
	tokenPrefix    = "ar1"
	tokenSecretLen = 32
)

var ErrInvalidToken = errors.New("invalid API token")

type IssuedToken struct {
	Bearer     string
	SecretHMAC []byte
}

func IssueToken(random io.Reader, tokenID uuid.UUID, pepper []byte) (IssuedToken, error) {
	if random == nil {
		return IssuedToken{}, fmt.Errorf("issue token: random reader is nil")
	}
	if tokenID == uuid.Nil {
		return IssuedToken{}, fmt.Errorf("issue token: token ID is nil")
	}
	if len(pepper) != 32 {
		return IssuedToken{}, fmt.Errorf("issue token: pepper must be 32 bytes")
	}

	secret := make([]byte, tokenSecretLen)
	if _, err := io.ReadFull(random, secret); err != nil {
		return IssuedToken{}, fmt.Errorf("issue token: read secret: %w", err)
	}
	bearer := tokenPrefix + "." + tokenID.String() + "." + base64.RawURLEncoding.EncodeToString(secret)
	return IssuedToken{
		Bearer:     bearer,
		SecretHMAC: secretHMAC(pepper, tokenID, secret),
	}, nil
}

func ParseBearer(bearer string) (uuid.UUID, []byte, error) {
	parts := strings.Split(bearer, ".")
	if len(parts) != 3 || parts[0] != tokenPrefix {
		return uuid.Nil, nil, ErrInvalidToken
	}
	tokenID, err := uuid.Parse(parts[1])
	if err != nil || tokenID == uuid.Nil {
		return uuid.Nil, nil, ErrInvalidToken
	}
	secret, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(secret) != tokenSecretLen ||
		base64.RawURLEncoding.EncodeToString(secret) != parts[2] {
		return uuid.Nil, nil, ErrInvalidToken
	}
	return tokenID, secret, nil
}

func VerifySecret(pepper []byte, tokenID uuid.UUID, secret, expected []byte) bool {
	if len(pepper) != 32 || tokenID == uuid.Nil || len(secret) != tokenSecretLen ||
		len(expected) != sha256.Size {
		return false
	}
	actual := secretHMAC(pepper, tokenID, secret)
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

func secretHMAC(pepper []byte, tokenID uuid.UUID, secret []byte) []byte {
	mac := hmac.New(sha256.New, pepper)
	_, _ = mac.Write(tokenID[:])
	_, _ = mac.Write(secret)
	return mac.Sum(nil)
}
