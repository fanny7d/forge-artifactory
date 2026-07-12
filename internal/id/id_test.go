package id

import (
	"testing"

	"github.com/google/uuid"
)

func TestUUIDGeneratorReturnsDistinctVersion4IDs(t *testing.T) {
	generator := UUIDGenerator{}
	first := generator.New()
	second := generator.New()

	if first == uuid.Nil || second == uuid.Nil {
		t.Fatal("New() returned nil UUID")
	}
	if first == second {
		t.Fatalf("New() returned duplicate UUID %s", first)
	}
	if first.Version() != 4 || second.Version() != 4 {
		t.Fatalf("versions = %d and %d, want 4", first.Version(), second.Version())
	}
}
