package idempotency

import (
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"io"
)

type Sealer struct {
	aead   cipher.AEAD
	random io.Reader
}

func NewSealer(key []byte, random io.Reader) (*Sealer, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("idempotency response key must be 32 bytes")
	}
	if random == nil {
		return nil, fmt.Errorf("idempotency nonce reader is nil")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create AES-GCM: %w", err)
	}
	return &Sealer{aead: aead, random: random}, nil
}

func (s *Sealer) Seal(plaintext, aad []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(s.random, nonce); err != nil {
		return nil, fmt.Errorf("read AES-GCM nonce: %w", err)
	}
	return s.aead.Seal(nonce, nonce, plaintext, aad), nil
}

func (s *Sealer) Open(sealed, aad []byte) ([]byte, error) {
	nonceSize := s.aead.NonceSize()
	if len(sealed) < nonceSize {
		return nil, fmt.Errorf("sealed idempotency response is truncated")
	}
	plaintext, err := s.aead.Open(nil, sealed[:nonceSize], sealed[nonceSize:], aad)
	if err != nil {
		return nil, fmt.Errorf("open idempotency response: %w", err)
	}
	return plaintext, nil
}
