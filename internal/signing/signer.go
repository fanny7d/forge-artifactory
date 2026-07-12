package signing

import (
	"context"
	"crypto/ed25519"
)

type Signer interface {
	KeyID() string
	Sign(context.Context, []byte) ([]byte, error)
	PublicKey() ed25519.PublicKey
}
