package auth

import (
	"errors"

	"github.com/google/uuid"
)

var (
	ErrForbidden        = errors.New("forbidden")
	ErrRepositoryDenied = errors.New("repository access denied")
)

type Scope string

const (
	ScopeArtifactRead   Scope = "artifact:read"
	ScopeArtifactWrite  Scope = "artifact:write"
	ScopeReleasePublish Scope = "release:publish"
	ScopeChannelPromote Scope = "channel:promote"
	ScopeAdmin          Scope = "admin"
)

type ScopeSet map[Scope]struct{}

func NewScopeSet(scopes ...Scope) ScopeSet {
	set := make(ScopeSet, len(scopes))
	for _, scope := range scopes {
		set[scope] = struct{}{}
	}
	return set
}

func (s ScopeSet) Has(scope Scope) bool {
	_, ok := s[scope]
	return ok
}

type Actor struct {
	TokenID            uuid.UUID
	ServiceAccountID   uuid.UUID
	ServiceAccountName string
	Scopes             ScopeSet
	RepositoryIDs      map[uuid.UUID]struct{}
}

func Require(actor Actor, scope Scope, repositoryID uuid.UUID) error {
	if actor.Scopes.Has(ScopeAdmin) {
		return nil
	}
	if !actor.Scopes.Has(scope) {
		return ErrForbidden
	}
	if repositoryID == uuid.Nil {
		return nil
	}
	if _, ok := actor.RepositoryIDs[repositoryID]; !ok {
		return ErrForbidden
	}
	return nil
}
