package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	identity "superfan.myasustor.com/fanchao/artifact-repository/internal/auth"
	releasedomain "superfan.myasustor.com/fanchao/artifact-repository/internal/release"
	repositorydomain "superfan.myasustor.com/fanchao/artifact-repository/internal/repository"
)

func TestRepositoryListEncodesServiceCursor(t *testing.T) {
	authenticator := &identityServiceStub{actor: adminAPIActor()}
	repositories := &repositoryServiceStub{listResult: repositorydomain.Page{
		Items: []repositorydomain.Repository{{
			ID: uuid.MustParse("11111111-2222-4333-8444-555555555555"), Key: "alpha",
			DisplayName: "Alpha", Type: "local/raw", CreatedAt: time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC),
		}},
		Next: &repositorydomain.Cursor{Key: "alpha"},
	}}
	handler := NewServer(Dependencies{
		Readiness: &readinessProbe{}, Authenticator: authenticator, Repositories: repositories,
	})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/repositories?limit=1", nil)
	request.Header.Set("Authorization", "Bearer ar1.valid")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var page RepositoryPage
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Key != "alpha" || page.NextCursor == nil {
		t.Fatalf("page = %+v", page)
	}
	if repositories.listRequest.Limit != 1 || repositories.listActor.TokenID != authenticator.actor.TokenID {
		t.Fatalf("List() actor/request = %+v, %+v", repositories.listActor, repositories.listRequest)
	}
}

func TestPackageAndDraftMutationHandlersUseFullyScopedCanonicalResources(t *testing.T) {
	authenticator := &identityServiceStub{actor: adminAPIActor()}
	packageID := uuid.MustParse("22222222-3333-4444-8555-666666666666")
	releaseID := uuid.MustParse("33333333-4444-4555-8666-777777777777")
	releaseArtifactID := uuid.MustParse("44444444-5555-4666-8777-888888888888")
	packages := &packageServiceStub{createResult: releasedomain.Package{
		ID: packageID, RepositoryKey: "releases", Name: "edgecli", Channels: []string{"candidate", "stable"},
		CreatedAt: time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC),
	}}
	drafts := &draftServiceStub{
		createResult: releasedomain.Release{
			ID: releaseID, RepositoryKey: "releases", PackageName: "edgecli", Version: "1.2.3", State: "draft",
			Artifacts: []releasedomain.ReleaseArtifact{}, CreatedAt: time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC),
		},
		addResult: releasedomain.ReleaseArtifact{
			ID: releaseArtifactID,
			Artifact: releasedomain.Artifact{
				ID: uuid.MustParse("55555555-6666-4777-8888-999999999999"), RepositoryKey: "releases",
				Path: "linux/arm64/edgecli", Filename: "edgecli", MediaType: "application/octet-stream",
				SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Properties: map[string]any{},
				CreatedAt: time.Date(2026, 7, 11, 11, 0, 0, 0, time.UTC),
			},
			OS: "linux", Arch: "arm64", Role: "binary",
		},
	}
	handler := NewServer(Dependencies{
		Readiness: &readinessProbe{}, Authenticator: authenticator, Packages: packages, Drafts: drafts,
	})

	requests := []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
	}{
		{name: "package", method: http.MethodPost, path: "/api/v1/repositories/releases/packages", body: `{"name":"edgecli"}`, wantStatus: http.StatusCreated},
		{name: "release", method: http.MethodPost, path: "/api/v1/repositories/releases/packages/edgecli/releases", body: `{"version":"1.2.3"}`, wantStatus: http.StatusCreated},
		{name: "add artifact", method: http.MethodPost, path: "/api/v1/repositories/releases/packages/edgecli/releases/1.2.3/artifacts", body: `{"artifactPath":"linux/arm64/edgecli","os":"linux","arch":"arm64","role":"binary"}`, wantStatus: http.StatusCreated},
		{name: "remove artifact", method: http.MethodDelete, path: "/api/v1/repositories/releases/packages/edgecli/releases/1.2.3/artifacts/" + releaseArtifactID.String(), wantStatus: http.StatusNoContent},
		{name: "cancel release", method: http.MethodDelete, path: "/api/v1/repositories/releases/packages/edgecli/releases/1.2.3", wantStatus: http.StatusNoContent},
	}
	for _, tt := range requests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(tt.method, tt.path, bytes.NewBufferString(tt.body))
			request.Header.Set("Authorization", "Bearer ar1.valid")
			request.Header.Set("Idempotency-Key", "request-key")
			if tt.body != "" {
				request.Header.Set("Content-Type", "application/json")
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, tt.wantStatus, response.Body.String())
			}
		})
	}

	if packages.createRequest.Mutation.CanonicalResource != "/api/v1/repositories/releases/packages" {
		t.Fatalf("package mutation = %+v", packages.createRequest.Mutation)
	}
	if drafts.createRequest.Mutation.CanonicalResource != "/api/v1/repositories/releases/packages/edgecli/releases" {
		t.Fatalf("release mutation = %+v", drafts.createRequest.Mutation)
	}
	if drafts.addRequest.Mutation.CanonicalResource != "/api/v1/repositories/releases/packages/edgecli/releases/1.2.3/artifacts" {
		t.Fatalf("add mutation = %+v", drafts.addRequest.Mutation)
	}
	if drafts.removeRequest.ReleaseArtifactID != releaseArtifactID || drafts.removeRequest.Mutation.IdempotencyKey != "request-key" || drafts.removeRequest.Mutation.Method != http.MethodDelete {
		t.Fatalf("remove request = %+v", drafts.removeRequest)
	}
	if drafts.cancelRequest.Mutation.CanonicalResource != "/api/v1/repositories/releases/packages/edgecli/releases/1.2.3" || drafts.cancelRequest.Mutation.Method != http.MethodDelete {
		t.Fatalf("cancel request = %+v", drafts.cancelRequest)
	}
}

type repositoryServiceStub struct {
	listActor   identity.Actor
	listRequest repositorydomain.ListRequest
	listResult  repositorydomain.Page
	getError    error
}

func (s *repositoryServiceStub) Create(context.Context, repositorydomain.CreateRequest) (repositorydomain.Repository, error) {
	return repositorydomain.Repository{}, nil
}

func (s *repositoryServiceStub) Get(context.Context, identity.Actor, string) (repositorydomain.Repository, error) {
	return repositorydomain.Repository{}, s.getError
}

func (s *repositoryServiceStub) List(_ context.Context, actor identity.Actor, request repositorydomain.ListRequest) (repositorydomain.Page, error) {
	s.listActor = actor
	s.listRequest = request
	return s.listResult, nil
}

type packageServiceStub struct {
	createRequest releasedomain.CreatePackageRequest
	createResult  releasedomain.Package
	createError   error
}

func (s *packageServiceStub) Create(_ context.Context, request releasedomain.CreatePackageRequest) (releasedomain.Package, error) {
	s.createRequest = request
	return s.createResult, s.createError
}

func (s *packageServiceStub) Get(context.Context, identity.Actor, string, string) (releasedomain.Package, error) {
	return releasedomain.Package{}, nil
}

func (s *packageServiceStub) List(context.Context, identity.Actor, string, releasedomain.PackageListRequest) (releasedomain.PackagePage, error) {
	return releasedomain.PackagePage{}, nil
}

type draftServiceStub struct {
	createRequest releasedomain.CreateDraftRequest
	createResult  releasedomain.Release
	addRequest    releasedomain.AddArtifactRequest
	addResult     releasedomain.ReleaseArtifact
	removeRequest releasedomain.RemoveArtifactRequest
	cancelRequest releasedomain.CancelDraftRequest
}

func (s *draftServiceStub) Create(_ context.Context, request releasedomain.CreateDraftRequest) (releasedomain.Release, error) {
	s.createRequest = request
	return s.createResult, nil
}

func (s *draftServiceStub) Get(context.Context, identity.Actor, string, string, string) (releasedomain.Release, error) {
	return releasedomain.Release{}, nil
}

func (s *draftServiceStub) List(context.Context, identity.Actor, string, string, releasedomain.ReleaseListRequest) (releasedomain.ReleasePage, error) {
	return releasedomain.ReleasePage{}, nil
}

func (s *draftServiceStub) AddArtifact(_ context.Context, request releasedomain.AddArtifactRequest) (releasedomain.ReleaseArtifact, error) {
	s.addRequest = request
	return s.addResult, nil
}

func (s *draftServiceStub) RemoveArtifact(_ context.Context, request releasedomain.RemoveArtifactRequest) (releasedomain.RemoveArtifactResult, error) {
	s.removeRequest = request
	return releasedomain.RemoveArtifactResult{}, nil
}

func (s *draftServiceStub) Cancel(_ context.Context, request releasedomain.CancelDraftRequest) (releasedomain.CancelDraftResult, error) {
	s.cancelRequest = request
	return releasedomain.CancelDraftResult{}, nil
}
