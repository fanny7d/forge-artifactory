package auth

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestRequireScopeAndRepositoryDefaultsToDeny(t *testing.T) {
	repositoryID := uuid.New()
	actor := Actor{
		Scopes:        NewScopeSet(ScopeArtifactRead),
		RepositoryIDs: map[uuid.UUID]struct{}{},
	}

	if err := Require(actor, ScopeArtifactRead, repositoryID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("Require() error = %v, want ErrForbidden", err)
	}
}

func TestRequireAllowsMatchingScopeAndRepository(t *testing.T) {
	repositoryID := uuid.New()
	actor := Actor{
		Scopes:        NewScopeSet(ScopeArtifactRead),
		RepositoryIDs: map[uuid.UUID]struct{}{repositoryID: {}},
	}

	if err := Require(actor, ScopeArtifactRead, repositoryID); err != nil {
		t.Fatalf("Require() error = %v", err)
	}
}

func TestAdminBypassesScopeAndRepositoryChecks(t *testing.T) {
	actor := Actor{Scopes: NewScopeSet(ScopeAdmin)}
	if err := Require(actor, ScopeChannelPromote, uuid.New()); err != nil {
		t.Fatalf("Require() error = %v", err)
	}
}
