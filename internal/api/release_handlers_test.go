package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	identity "superfan.myasustor.com/fanchao/artifact-repository/internal/auth"
	releasedomain "superfan.myasustor.com/fanchao/artifact-repository/internal/release"
)

func TestPublishAndManifestHandlersUseScopedRoutesAndBase64URL(t *testing.T) {
	authenticator := &identityServiceStub{actor: adminAPIActor()}
	publishedAt := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	service := &publishServiceStub{
		publishResult: releasedomain.PublishedRelease{Release: releasedomain.Release{
			ID:            uuid.MustParse("77777777-8888-4999-8aaa-bbbbbbbbbbbb"),
			RepositoryKey: "releases",
			PackageName:   "edgecli",
			Version:       "1.2.3",
			State:         "published",
			Artifacts:     []releasedomain.ReleaseArtifact{},
			PublishedAt:   &publishedAt,
			CreatedAt:     publishedAt.Add(-time.Hour),
		}},
		manifestResult: releasedomain.SignedManifest{
			Version:        "1.2.3",
			Manifest:       []byte{0xfb, 0xff, 0x00},
			ManifestSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			KeyID:          "ed25519:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			Signature:      []byte{0xfa, 0x10, 0xff},
		},
	}
	handler := NewServer(Dependencies{
		Readiness:     &readinessProbe{},
		Authenticator: authenticator,
		Publisher:     service,
	})

	publishRequest := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/repositories/releases/packages/edgecli/releases/1.2.3/publish",
		http.NoBody,
	)
	publishRequest.Header.Set("Authorization", "Bearer ar1.valid")
	publishRequest.Header.Set("Idempotency-Key", "publish-1.2.3")
	publishResponse := httptest.NewRecorder()
	handler.ServeHTTP(publishResponse, publishRequest)
	if publishResponse.Code != http.StatusOK {
		t.Fatalf("publish status = %d, body = %s", publishResponse.Code, publishResponse.Body.String())
	}
	if service.publishCommand.Mutation.CanonicalResource != "/api/v1/repositories/releases/packages/edgecli/releases/1.2.3/publish" ||
		service.publishCommand.Mutation.IdempotencyKey != "publish-1.2.3" ||
		service.publishCommand.Mutation.Method != http.MethodPost {
		t.Fatalf("publish command = %+v", service.publishCommand)
	}
	var release Release
	if err := json.Unmarshal(publishResponse.Body.Bytes(), &release); err != nil {
		t.Fatalf("decode publish response: %v", err)
	}
	if release.State != Published || release.Version != "1.2.3" {
		t.Fatalf("published release = %+v", release)
	}

	manifestRequest := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/repositories/releases/packages/edgecli/releases/1.2.3/manifest",
		nil,
	)
	manifestRequest.Header.Set("Authorization", "Bearer ar1.valid")
	manifestResponse := httptest.NewRecorder()
	handler.ServeHTTP(manifestResponse, manifestRequest)
	if manifestResponse.Code != http.StatusOK {
		t.Fatalf("manifest status = %d, body = %s", manifestResponse.Code, manifestResponse.Body.String())
	}
	var manifest ReleaseManifest
	if err := json.Unmarshal(manifestResponse.Body.Bytes(), &manifest); err != nil {
		t.Fatalf("decode manifest response: %v", err)
	}
	if manifest.Manifest != base64.RawURLEncoding.EncodeToString(service.manifestResult.Manifest) ||
		manifest.Signature != base64.RawURLEncoding.EncodeToString(service.manifestResult.Signature) ||
		manifest.ManifestSha256 != service.manifestResult.ManifestSHA256 {
		t.Fatalf("manifest response = %+v", manifest)
	}
	if service.manifestRepository != "releases" || service.manifestPackage != "edgecli" || service.manifestVersion != "1.2.3" || service.manifestActor.TokenID != authenticator.actor.TokenID {
		t.Fatalf("manifest request = actor %+v, %s/%s/%s", service.manifestActor, service.manifestRepository, service.manifestPackage, service.manifestVersion)
	}
}

func TestPublishHandlerRejectsBodyAndMapsPendingRecovery(t *testing.T) {
	authenticator := &identityServiceStub{actor: adminAPIActor()}
	service := &publishServiceStub{publishError: releasedomain.ErrPublishPending}
	handler := NewServer(Dependencies{
		Readiness:     &readinessProbe{},
		Authenticator: authenticator,
		Publisher:     service,
	})
	path := "/api/v1/repositories/releases/packages/edgecli/releases/1.2.3/publish"

	withBody := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
	withBody.Header.Set("Authorization", "Bearer ar1.valid")
	withBodyResponse := httptest.NewRecorder()
	handler.ServeHTTP(withBodyResponse, withBody)
	if withBodyResponse.Code != http.StatusBadRequest || service.publishCalls != 0 {
		t.Fatalf("body publish = status %d calls %d body %s", withBodyResponse.Code, service.publishCalls, withBodyResponse.Body.String())
	}

	pending := httptest.NewRequest(http.MethodPost, path, http.NoBody)
	pending.Header.Set("Authorization", "Bearer ar1.valid")
	pendingResponse := httptest.NewRecorder()
	handler.ServeHTTP(pendingResponse, pending)
	if pendingResponse.Code != http.StatusServiceUnavailable || pendingResponse.Header().Get("Retry-After") != "5" {
		t.Fatalf("pending publish = status %d retry %q body %s", pendingResponse.Code, pendingResponse.Header().Get("Retry-After"), pendingResponse.Body.String())
	}
	var problem Problem
	if err := json.Unmarshal(pendingResponse.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode pending problem: %v", err)
	}
	if problem.Code != "publish-pending" || service.publishCalls != 1 {
		t.Fatalf("pending problem = %+v, calls = %d", problem, service.publishCalls)
	}
}

type publishServiceStub struct {
	publishCommand     releasedomain.PublishCommand
	publishResult      releasedomain.PublishedRelease
	publishError       error
	publishCalls       int
	manifestActor      identity.Actor
	manifestRepository string
	manifestPackage    string
	manifestVersion    string
	manifestResult     releasedomain.SignedManifest
}

func (s *publishServiceStub) Publish(_ context.Context, command releasedomain.PublishCommand) (releasedomain.PublishedRelease, error) {
	s.publishCalls++
	s.publishCommand = command
	return s.publishResult, s.publishError
}

func (s *publishServiceStub) GetManifest(_ context.Context, actor identity.Actor, repository, packageName, version string) (releasedomain.SignedManifest, error) {
	s.manifestActor = actor
	s.manifestRepository = repository
	s.manifestPackage = packageName
	s.manifestVersion = version
	return s.manifestResult, nil
}
