package id

import "github.com/google/uuid"

type Generator interface {
	New() uuid.UUID
}

type UUIDGenerator struct{}

func (UUIDGenerator) New() uuid.UUID {
	return uuid.New()
}
