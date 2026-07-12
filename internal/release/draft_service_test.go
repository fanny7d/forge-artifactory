package release

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/audit"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/auth"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/idempotency"
)

func TestDraftLifecycleReplaysMutationsAndCancelReleasesVersion(t *testing.T) {
	pool, service, actor, artifactID := newDraftTestService(t)
	createRequest := CreateDraftRequest{
		Mutation:      draftMutation(actor, "/api/v1/repositories/repo-a/packages/edgecli/releases", "create-release"),
		RepositoryKey: "repo-a",
		PackageName:   "edgecli",
		Version:       "1.2.3",
	}
	created, err := service.Create(t.Context(), createRequest)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	replayed, err := service.Create(t.Context(), createRequest)
	if err != nil {
		t.Fatalf("Create() replay error = %v", err)
	}
	if created.ID != replayed.ID || created.Replayed || !replayed.Replayed {
		t.Fatalf("created = %+v, replay = %+v", created, replayed)
	}

	addRequest := AddArtifactRequest{
		Mutation:      draftMutation(actor, "/api/v1/repositories/repo-a/packages/edgecli/releases/1.2.3/artifacts", "add-artifact"),
		RepositoryKey: "repo-a",
		PackageName:   "edgecli",
		Version:       "1.2.3",
		ArtifactPath:  "linux/arm64/edgecli",
		OS:            "linux",
		Arch:          "arm64",
		Role:          "binary",
	}
	added, err := service.AddArtifact(t.Context(), addRequest)
	if err != nil {
		t.Fatalf("AddArtifact() error = %v", err)
	}
	addedReplay, err := service.AddArtifact(t.Context(), addRequest)
	if err != nil {
		t.Fatalf("AddArtifact() replay error = %v", err)
	}
	if added.Artifact.ID != artifactID || added.ID != addedReplay.ID || added.Replayed || !addedReplay.Replayed {
		t.Fatalf("added = %+v, replay = %+v", added, addedReplay)
	}
	coordinateConflict := addRequest
	coordinateConflict.Mutation = draftMutation(actor, addRequest.Mutation.CanonicalResource, "add-coordinate-conflict")
	if _, err := service.AddArtifact(t.Context(), coordinateConflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("AddArtifact() duplicate coordinate error = %v, want ErrConflict", err)
	} else if completed, ok := idempotency.CompletedErrorFrom(err); !ok || completed.Replayed || completed.Status != 409 {
		t.Fatalf("AddArtifact() duplicate coordinate completed error = %+v, err = %v", completed, err)
	}
	if _, err := service.AddArtifact(t.Context(), coordinateConflict); err == nil {
		t.Fatal("AddArtifact() duplicate coordinate replay returned nil error")
	} else if completed, ok := idempotency.CompletedErrorFrom(err); !ok || !completed.Replayed || completed.Status != 409 {
		t.Fatalf("AddArtifact() duplicate coordinate replay = %+v, err = %v", completed, err)
	}

	reader := actor
	reader.Scopes = auth.NewScopeSet(auth.ScopeArtifactRead)
	got, err := service.Get(t.Context(), reader, "repo-a", "edgecli", "1.2.3")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if len(got.Artifacts) != 1 || got.Artifacts[0].ID != added.ID {
		t.Fatalf("Get() release = %+v", got)
	}

	removeRequest := RemoveArtifactRequest{
		Mutation:          draftMutation(actor, "/api/v1/repositories/repo-a/packages/edgecli/releases/1.2.3/artifacts/"+added.ID.String(), "remove-artifact"),
		RepositoryKey:     "repo-a",
		PackageName:       "edgecli",
		Version:           "1.2.3",
		ReleaseArtifactID: added.ID,
	}
	removed, err := service.RemoveArtifact(t.Context(), removeRequest)
	if err != nil {
		t.Fatalf("RemoveArtifact() error = %v", err)
	}
	removedReplay, err := service.RemoveArtifact(t.Context(), removeRequest)
	if err != nil {
		t.Fatalf("RemoveArtifact() replay error = %v", err)
	}
	if removed.Replayed || !removedReplay.Replayed {
		t.Fatalf("removed = %+v, replay = %+v", removed, removedReplay)
	}

	addRequest.Mutation = draftMutation(actor, addRequest.Mutation.CanonicalResource, "add-artifact-again")
	addedAgain, err := service.AddArtifact(t.Context(), addRequest)
	if err != nil {
		t.Fatalf("AddArtifact() again error = %v", err)
	}
	cancelRequest := CancelDraftRequest{
		Mutation:      draftMutation(actor, "/api/v1/repositories/repo-a/packages/edgecli/releases/1.2.3", "cancel-release"),
		RepositoryKey: "repo-a",
		PackageName:   "edgecli",
		Version:       "1.2.3",
	}
	cancelled, err := service.Cancel(t.Context(), cancelRequest)
	if err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}
	cancelledReplay, err := service.Cancel(t.Context(), cancelRequest)
	if err != nil {
		t.Fatalf("Cancel() replay error = %v", err)
	}
	if cancelled.Replayed || !cancelledReplay.Replayed {
		t.Fatalf("cancelled = %+v, replay = %+v", cancelled, cancelledReplay)
	}
	var releaseArtifacts int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM release_artifacts WHERE id = $1", addedAgain.ID).Scan(&releaseArtifacts); err != nil {
		t.Fatalf("count release artifacts: %v", err)
	}
	if releaseArtifacts != 0 {
		t.Fatalf("release artifacts after cancel = %d", releaseArtifacts)
	}

	createRequest.Mutation = draftMutation(actor, createRequest.Mutation.CanonicalResource, "recreate-release")
	recreated, err := service.Create(t.Context(), createRequest)
	if err != nil {
		t.Fatalf("Create() after cancel error = %v", err)
	}
	if recreated.ID == created.ID {
		t.Fatalf("recreated ID = %s, want a new release", recreated.ID)
	}
}

func TestListDraftsUsesStableCursorAndIncludesArtifacts(t *testing.T) {
	_, service, actor, _ := newDraftTestService(t)
	for index, version := range []string{"1.0.0", "2.0.0"} {
		if _, err := service.Create(t.Context(), CreateDraftRequest{
			Mutation:      draftMutation(actor, "/api/v1/repositories/repo-a/packages/edgecli/releases", "create-list-"+version),
			RepositoryKey: "repo-a", PackageName: "edgecli", Version: version,
		}); err != nil {
			t.Fatalf("Create(%s) error = %v", version, err)
		}
		if index == 1 {
			if _, err := service.AddArtifact(t.Context(), AddArtifactRequest{
				Mutation:      draftMutation(actor, "/api/v1/repositories/repo-a/packages/edgecli/releases/2.0.0/artifacts", "add-list-artifact"),
				RepositoryKey: "repo-a", PackageName: "edgecli", Version: "2.0.0",
				ArtifactPath: "linux/arm64/edgecli", OS: "linux", Arch: "arm64",
			}); err != nil {
				t.Fatalf("AddArtifact() error = %v", err)
			}
		}
	}
	reader := actor
	reader.Scopes = auth.NewScopeSet(auth.ScopeArtifactRead)
	first, err := service.List(t.Context(), reader, "repo-a", "edgecli", ReleaseListRequest{Limit: 1})
	if err != nil {
		t.Fatalf("List() first page error = %v", err)
	}
	second, err := service.List(t.Context(), reader, "repo-a", "edgecli", ReleaseListRequest{Limit: 1, After: first.Next})
	if err != nil {
		t.Fatalf("List() second page error = %v", err)
	}
	if len(first.Items) != 1 || first.Next == nil || len(second.Items) != 1 || first.Items[0].ID == second.Items[0].ID {
		t.Fatalf("release pages = %+v, %+v", first, second)
	}
	var withArtifact Release
	if len(first.Items[0].Artifacts) == 1 {
		withArtifact = first.Items[0]
	} else {
		withArtifact = second.Items[0]
	}
	if len(withArtifact.Artifacts) != 1 || withArtifact.Artifacts[0].Artifact.Properties["kind"] != "binary" {
		t.Fatalf("release with artifact = %+v", withArtifact)
	}
}

func TestDraftMutationsRejectReleaseAfterItLeavesDraft(t *testing.T) {
	pool, service, actor, _ := newDraftTestService(t)
	created, err := service.Create(t.Context(), CreateDraftRequest{
		Mutation:      draftMutation(actor, "/api/v1/repositories/repo-a/packages/edgecli/releases", "create-frozen"),
		RepositoryKey: "repo-a",
		PackageName:   "edgecli",
		Version:       "2.0.0",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	added, err := service.AddArtifact(t.Context(), AddArtifactRequest{
		Mutation:      draftMutation(actor, "/api/v1/repositories/repo-a/packages/edgecli/releases/2.0.0/artifacts", "add-before-freeze"),
		RepositoryKey: "repo-a",
		PackageName:   "edgecli",
		Version:       "2.0.0",
		ArtifactPath:  "linux/arm64/edgecli",
		OS:            "linux",
		Arch:          "arm64",
	})
	if err != nil {
		t.Fatalf("AddArtifact() before freeze error = %v", err)
	}
	if _, err := pool.Exec(t.Context(), "UPDATE releases SET state = 'publishing' WHERE id = $1", created.ID); err != nil {
		t.Fatalf("freeze release: %v", err)
	}

	tests := []struct {
		name string
		run  func() error
	}{
		{
			name: "add",
			run: func() error {
				_, err := service.AddArtifact(t.Context(), AddArtifactRequest{
					Mutation:      draftMutation(actor, "/api/v1/repositories/repo-a/packages/edgecli/releases/2.0.0/artifacts", "add-after-freeze"),
					RepositoryKey: "repo-a", PackageName: "edgecli", Version: "2.0.0",
					ArtifactPath: "linux/arm64/edgecli", OS: "linux", Arch: "arm64", Variant: "musl",
				})
				return err
			},
		},
		{
			name: "remove",
			run: func() error {
				_, err := service.RemoveArtifact(t.Context(), RemoveArtifactRequest{
					Mutation:      draftMutation(actor, "/api/v1/repositories/repo-a/packages/edgecli/releases/2.0.0/artifacts/"+added.ID.String(), "remove-after-freeze"),
					RepositoryKey: "repo-a", PackageName: "edgecli", Version: "2.0.0", ReleaseArtifactID: added.ID,
				})
				return err
			},
		},
		{
			name: "cancel",
			run: func() error {
				_, err := service.Cancel(t.Context(), CancelDraftRequest{
					Mutation:      draftMutation(actor, "/api/v1/repositories/repo-a/packages/edgecli/releases/2.0.0", "cancel-after-freeze"),
					RepositoryKey: "repo-a", PackageName: "edgecli", Version: "2.0.0",
				})
				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.run(); !errors.Is(err, ErrConflict) {
				t.Fatalf("mutation error = %v, want ErrConflict", err)
			} else if completed, ok := idempotency.CompletedErrorFrom(err); !ok || completed.Replayed || completed.Status != 409 {
				t.Fatalf("mutation completed error = %+v, err = %v", completed, err)
			}
		})
	}
	var failedAudits int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events
		WHERE request_id = 'request-draft' AND outcome = 'failed' AND code = 'conflict'
		  AND action IN ('release-artifact.add', 'release-artifact.remove', 'release.cancel')`).Scan(&failedAudits); err != nil {
		t.Fatalf("count frozen mutation audits: %v", err)
	}
	if failedAudits != 3 {
		t.Fatalf("frozen mutation audits = %d, want 3", failedAudits)
	}
}

func TestAddArtifactRejectsVisibleArtifactFromAnotherRepository(t *testing.T) {
	pool, service, actor, _ := newDraftTestService(t)
	if _, err := service.Create(t.Context(), CreateDraftRequest{
		Mutation:      draftMutation(actor, "/api/v1/repositories/repo-a/packages/edgecli/releases", "create-cross-repo"),
		RepositoryKey: "repo-a", PackageName: "edgecli", Version: "3.0.0",
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	otherBlob := bytes.Repeat([]byte{0x22}, 32)
	otherSHA := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := pool.Exec(t.Context(), "INSERT INTO blobs (sha256, size, object_key, state) VALUES ($1, $2, $3, 'ready')", otherSHA, len(otherBlob), "blobs/"+otherSHA); err != nil {
		t.Fatalf("insert other blob: %v", err)
	}
	var otherRepositoryID uuid.UUID
	if err := pool.QueryRow(t.Context(), "SELECT id FROM repositories WHERE key = 'repo-b'").Scan(&otherRepositoryID); err != nil {
		t.Fatalf("get repo-b: %v", err)
	}
	if _, err := pool.Exec(
		t.Context(),
		`INSERT INTO artifacts
		 (repository_id, logical_path, blob_sha256, media_type, filename, properties, created_by)
		 VALUES ($1, 'foreign/edgecli', $2, 'application/octet-stream', 'edgecli', '{}', $3)`,
		otherRepositoryID, otherSHA, actor.TokenID,
	); err != nil {
		t.Fatalf("insert foreign artifact: %v", err)
	}

	request := AddArtifactRequest{
		Mutation:      draftMutation(actor, "/api/v1/repositories/repo-a/packages/edgecli/releases/3.0.0/artifacts", "add-cross-repo"),
		RepositoryKey: "repo-a", PackageName: "edgecli", Version: "3.0.0",
		ArtifactPath: "foreign/edgecli", OS: "linux", Arch: "arm64",
	}
	_, err := service.AddArtifact(t.Context(), request)
	if !errors.Is(err, ErrUnprocessable) {
		t.Fatalf("AddArtifact() error = %v, want ErrUnprocessable", err)
	}
	completed, ok := idempotency.CompletedErrorFrom(err)
	if !ok || completed.Replayed || completed.Status != 422 {
		t.Fatalf("AddArtifact() completed error = %+v, err = %v", completed, err)
	}
	var action, resourceType, outcome, code, details string
	var actorTokenID, repositoryID uuid.UUID
	err = pool.QueryRow(t.Context(), `
		SELECT action, resource_type, outcome, code, details::text, actor_token_id, repository_id
		FROM audit_events
		WHERE request_id = 'request-draft'
		  AND outcome = 'denied'
		  AND code = 'cross-repository-artifact'
		ORDER BY created_at DESC
		LIMIT 1`).Scan(&action, &resourceType, &outcome, &code, &details, &actorTokenID, &repositoryID)
	if err != nil {
		t.Fatalf("load cross-repository denial audit: %v", err)
	}
	var targetRepositoryID uuid.UUID
	if err := pool.QueryRow(t.Context(), "SELECT id FROM repositories WHERE key = 'repo-a'").Scan(&targetRepositoryID); err != nil {
		t.Fatalf("get repo-a: %v", err)
	}
	if action != "release-artifact.add" || resourceType != "release_artifact" || outcome != "denied" ||
		code != "cross-repository-artifact" || actorTokenID != actor.TokenID || repositoryID != targetRepositoryID {
		t.Fatalf("denial audit attribution = action=%q resource=%q outcome=%q code=%q actor=%s repository=%s",
			action, resourceType, outcome, code, actorTokenID, repositoryID)
	}
	if strings.Contains(details, "foreign/edgecli") {
		t.Fatalf("denial audit details leak foreign artifact path: %s", details)
	}
	var completedRecords int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM idempotency_records
		WHERE idempotency_key = 'add-cross-repo' AND state = 'completed' AND http_status = 422`).Scan(&completedRecords); err != nil {
		t.Fatalf("count completed denial records: %v", err)
	}
	if completedRecords != 1 {
		t.Fatalf("completed denial records = %d, want 1", completedRecords)
	}

	sealer, err := idempotency.NewSealer(bytes.Repeat([]byte{0x43}, 32), bytes.NewReader(bytes.Repeat([]byte{0x25}, 1024)))
	if err != nil {
		t.Fatalf("NewSealer() after restart error = %v", err)
	}
	restarted, err := NewDraftService(DraftServiceOptions{
		Pool:           pool,
		Idempotency:    idempotency.NewService(pool, sealer, func() time.Time { return packageTestTime }),
		Audit:          audit.NewService(pool),
		IdempotencyTTL: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewDraftService() after restart error = %v", err)
	}
	_, err = restarted.AddArtifact(t.Context(), request)
	replayed, ok := idempotency.CompletedErrorFrom(err)
	if !ok || !replayed.Replayed || replayed.Status != 422 {
		t.Fatalf("AddArtifact() replay error = %+v, err = %v", replayed, err)
	}
	var denialAudits int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events
		WHERE request_id = 'request-draft' AND outcome = 'denied' AND code = 'cross-repository-artifact'`).Scan(&denialAudits); err != nil {
		t.Fatalf("count denial audits after replay: %v", err)
	}
	if denialAudits != 1 {
		t.Fatalf("denial audits after replay = %d, want 1", denialAudits)
	}
}

func newDraftTestService(t *testing.T) (*pgxpool.Pool, *DraftService, auth.Actor, uuid.UUID) {
	t.Helper()
	pool, packageService, actor, repositories := newPackageTestService(t)
	if _, err := packageService.Create(t.Context(), CreatePackageRequest{
		Mutation:      packageMutation(actor, "repo-a", "setup-package"),
		RepositoryKey: "repo-a",
		Name:          "edgecli",
	}); err != nil {
		t.Fatalf("create setup package: %v", err)
	}
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := pool.Exec(t.Context(), "INSERT INTO blobs (sha256, size, object_key, state) VALUES ($1, 7, $2, 'ready')", sha, "blobs/"+sha); err != nil {
		t.Fatalf("insert blob: %v", err)
	}
	artifactID := uuid.MustParse("33333333-4444-4555-8666-777777777777")
	if _, err := pool.Exec(
		t.Context(),
		`INSERT INTO artifacts
		 (id, repository_id, logical_path, blob_sha256, media_type, filename, properties, created_by)
		 VALUES ($1, $2, 'linux/arm64/edgecli', $3, 'application/octet-stream', 'edgecli', '{"kind":"binary"}', $4)`,
		artifactID, repositories["repo-a"], sha, actor.TokenID,
	); err != nil {
		t.Fatalf("insert artifact: %v", err)
	}
	sealer, err := idempotency.NewSealer(bytes.Repeat([]byte{0x43}, 32), bytes.NewReader(bytes.Repeat([]byte{0x25}, 1024)))
	if err != nil {
		t.Fatalf("NewSealer() error = %v", err)
	}
	service, err := NewDraftService(DraftServiceOptions{
		Pool:           pool,
		Idempotency:    idempotency.NewService(pool, sealer, func() time.Time { return packageTestTime }),
		Audit:          audit.NewService(pool),
		IdempotencyTTL: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewDraftService() error = %v", err)
	}
	return pool, service, actor, artifactID
}

func draftMutation(actor auth.Actor, resource, key string) auth.Mutation {
	return auth.Mutation{
		Actor:             actor,
		RequestID:         "request-draft",
		IdempotencyKey:    key,
		Fingerprint:       bytes.Repeat([]byte{0x12}, 32),
		CanonicalResource: resource,
	}
}
