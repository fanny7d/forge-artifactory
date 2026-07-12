package e2e

import (
	"bytes"
	"context"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gowebpki/jcs"

	api "superfan.myasustor.com/fanchao/artifact-repository/internal/api"
)

func TestArtifactRepositoryMVP(t *testing.T) {
	if os.Getenv("ARTIFACT_REPOSITORY_E2E") != "1" {
		t.Skip("set ARTIFACT_REPOSITORY_E2E=1 to run the Docker Compose acceptance test")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()

	runID := uniqueRunID(t)
	keys := &idempotencyKeys{prefix: runID}
	project := environmentOrDefault("E2E_COMPOSE_PROJECT", "artifact-repository")
	baseURL := environmentOrDefault("E2E_BASE_URL", "http://127.0.0.1:8080")
	workerURL := environmentOrDefault("E2E_WORKER_URL", "http://127.0.0.1:8081")
	publicEndpoint := environmentOrDefault("E2E_PUBLIC_ENDPOINT", "http://localhost:9000")

	client, err := newAPIClient(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitForReady(ctx, baseURL, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := waitForReady(ctx, workerURL, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	adminToken, err := bootstrapAdmin(ctx, project, "admin-"+runID)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := trustedPublicKey(ctx, project)
	if err != nil {
		t.Fatal(err)
	}

	repoA := "repo-a-" + runID
	repoB := "repo-b-" + runID
	packageName := "package-" + runID
	otherPackage := "other-" + runID
	deniedPackage := "denied-" + runID
	versionOne := "1.0.0"
	versionTwo := "2.0.0"
	cancelVersion := "0.0.0-cancel"
	artifactOnePath := "linux/arm64/" + runID + "/edgecli-v1"
	artifactTwoPath := "linux/arm64/" + runID + "/edgecli-v2"
	concurrentPath := "linux/arm64/" + runID + "/concurrent"
	foreignPath := "private/arm64/" + runID + "/foreign"

	var (
		repositoryA       api.Repository
		publisherToken    api.IssuedToken
		readerToken       api.IssuedToken
		emptyToken        api.IssuedToken
		artifactOne       api.Artifact
		artifactTwo       api.Artifact
		releaseArtifact   api.ReleaseArtifact
		manifestOne       api.ReleaseManifest
		manifestOneBytes  []byte
		manifestSignature []byte
	)

	step(t, "authentication rejects a missing bearer", func(t *testing.T) {
		response := mustRequest(t, ctx, client, http.MethodGet, "/api/v1/repositories", "", nil, nil)
		requireProblem(t, response, http.StatusUnauthorized, "invalid-token")
	})

	step(t, "create two repositories", func(t *testing.T) {
		repositoryA, _ = requestJSON[api.Repository](
			t, ctx, client, http.MethodPost, "/api/v1/repositories", adminToken,
			api.CreateRepositoryRequest{Key: repoA, DisplayName: "E2E repository A"},
			keys.next("create-repo-a"), http.StatusCreated,
		)
		repositoryB, _ := requestJSON[api.Repository](
			t, ctx, client, http.MethodPost, "/api/v1/repositories", adminToken,
			api.CreateRepositoryRequest{Key: repoB, DisplayName: "E2E repository B"},
			keys.next("create-repo-b"), http.StatusCreated,
		)
		if repositoryA.Key != repoA || repositoryB.Key != repoB || repositoryA.Id == repositoryB.Id {
			t.Fatalf("created repositories do not match requested identities: A=%+v B=%+v", repositoryA, repositoryB)
		}
	})

	step(t, "create scoped publisher and replay its token secret", func(t *testing.T) {
		account := createServiceAccount(t, ctx, client, adminToken, "publisher-"+runID, keys.next("publisher-account"))
		expiresAt := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
		request := api.CreateTokenRequest{
			Scopes:       []api.Scope{api.ArtifactRead, api.ArtifactWrite, api.ReleasePublish, api.ChannelPromote},
			Repositories: []string{repoA, repoB},
			ExpiresAt:    expiresAt,
		}
		body := marshalJSON(t, request)
		path := "/api/v1/service-accounts/" + account.Id.String() + "/tokens"
		key := keys.next("publisher-token")
		var firstResponse apiResponse
		publisherToken, firstResponse = requestJSONBytes[api.IssuedToken](
			t, ctx, client, http.MethodPost, path, adminToken, body, key, http.StatusCreated,
		)
		replayed, replayResponse := requestJSONBytes[api.IssuedToken](
			t, ctx, client, http.MethodPost, path, adminToken, body, key, http.StatusCreated,
		)
		if replayed.Secret != publisherToken.Secret || replayed.Id != publisherToken.Id {
			t.Fatalf("token replay changed identity or one-time secret")
		}
		if len(publisherToken.Secret) != 84 || !strings.HasPrefix(publisherToken.Secret, "ar1.") {
			t.Fatalf("issued publisher token does not match the bearer shape")
		}
		if !bytes.Equal(firstResponse.Body, replayResponse.Body) {
			t.Fatalf("token replay response differs from the original response")
		}
		changed := request
		changed.Repositories = []string{repoA}
		conflict := mustRequest(
			t, ctx, client, http.MethodPost, path, adminToken, marshalJSON(t, changed),
			jsonMutationHeaders(key),
		)
		requireProblem(t, conflict, http.StatusConflict, "idempotency-key-conflict")
	})

	step(t, "create reader and empty-allowlist tokens", func(t *testing.T) {
		expiresAt := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
		readerAccount := createServiceAccount(t, ctx, client, adminToken, "reader-"+runID, keys.next("reader-account"))
		readerToken, _ = requestJSON[api.IssuedToken](
			t, ctx, client, http.MethodPost,
			"/api/v1/service-accounts/"+readerAccount.Id.String()+"/tokens", adminToken,
			api.CreateTokenRequest{Scopes: []api.Scope{api.ArtifactRead}, Repositories: []string{repoA}, ExpiresAt: expiresAt},
			keys.next("reader-token"), http.StatusCreated,
		)
		emptyAccount := createServiceAccount(t, ctx, client, adminToken, "empty-"+runID, keys.next("empty-account"))
		emptyToken, _ = requestJSON[api.IssuedToken](
			t, ctx, client, http.MethodPost,
			"/api/v1/service-accounts/"+emptyAccount.Id.String()+"/tokens", adminToken,
			api.CreateTokenRequest{Scopes: []api.Scope{api.ArtifactRead}, Repositories: []string{}, ExpiresAt: expiresAt},
			keys.next("empty-token"), http.StatusCreated,
		)
		response := mustRequest(t, ctx, client, http.MethodGet, "/api/v1/repositories/"+repoA, emptyToken.Secret, nil, nil)
		requireProblem(t, response, http.StatusNotFound, "not-found")
	})

	step(t, "reader cannot perform a write", func(t *testing.T) {
		response := mustRequest(
			t, ctx, client, http.MethodPost, "/api/v1/repositories/"+repoA+"/packages", readerToken.Secret,
			marshalJSON(t, api.CreatePackageRequest{Name: deniedPackage}),
			jsonMutationHeaders(keys.next("reader-write-denied")),
		)
		requireProblem(t, response, http.StatusForbidden, "forbidden")
	})

	step(t, "same package name is repository scoped and channels start empty", func(t *testing.T) {
		packageA, _ := requestJSON[api.Package](
			t, ctx, client, http.MethodPost, "/api/v1/repositories/"+repoA+"/packages", publisherToken.Secret,
			api.CreatePackageRequest{Name: packageName}, keys.next("package-a"), http.StatusCreated,
		)
		packageB, _ := requestJSON[api.Package](
			t, ctx, client, http.MethodPost, "/api/v1/repositories/"+repoB+"/packages", publisherToken.Secret,
			api.CreatePackageRequest{Name: packageName}, keys.next("package-b"), http.StatusCreated,
		)
		_, _ = requestJSON[api.Package](
			t, ctx, client, http.MethodPost, "/api/v1/repositories/"+repoA+"/packages", publisherToken.Secret,
			api.CreatePackageRequest{Name: otherPackage}, keys.next("other-package"), http.StatusCreated,
		)
		if packageA.Repository != repoA || packageB.Repository != repoB || packageA.Name != packageB.Name || packageA.Id == packageB.Id {
			t.Fatalf("same-name packages were not isolated by repository: A=%+v B=%+v", packageA, packageB)
		}
		for _, channel := range []string{"candidate", "stable"} {
			path := resolvePath(repoA, packageName, channel, false)
			response := mustRequest(t, ctx, client, http.MethodGet, path, readerToken.Secret, nil, nil)
			requireProblem(t, response, http.StatusNotFound, "not-found")
		}
	})

	artifactOneContent := append([]byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0}, []byte("artifact-repository-e2e-linux-arm64-v1-"+runID)...)
	artifactOneSHA := sha256Hex(artifactOneContent)

	step(t, "upload ARM64 bytes and checksum-deploy a second path", func(t *testing.T) {
		properties := base64.RawURLEncoding.EncodeToString(marshalJSON(t, map[string]any{
			"os": "linux", "arch": "arm64", "run": runID,
		}))
		response := mustRequest(
			t, ctx, client, http.MethodPut, artifactPath(repoA, artifactOnePath), publisherToken.Secret,
			artifactOneContent,
			map[string]string{
				"Content-Type":          "application/octet-stream",
				"X-Checksum-Sha256":     artifactOneSHA,
				"X-Artifact-Properties": properties,
			},
		)
		requireStatus(t, response, http.StatusCreated)
		decodeJSON(t, response.Body, &artifactOne)
		if artifactOne.Path != artifactOnePath || artifactOne.Sha256 != artifactOneSHA || artifactOne.Size != int64(len(artifactOneContent)) {
			t.Fatalf("uploaded artifact metadata = %+v", artifactOne)
		}
		response = mustRequest(
			t, ctx, client, http.MethodPut, artifactPath(repoA, artifactTwoPath), publisherToken.Secret,
			nil,
			map[string]string{
				"Content-Type":      "application/octet-stream",
				"X-Checksum-Sha256": artifactOneSHA,
				"X-Checksum-Deploy": "true",
			},
		)
		requireStatus(t, response, http.StatusCreated)
		decodeJSON(t, response.Body, &artifactTwo)
		if artifactTwo.Path != artifactTwoPath || artifactTwo.Sha256 != artifactOneSHA || artifactTwo.Size != artifactOne.Size {
			t.Fatalf("checksum-deployed artifact metadata = %+v", artifactTwo)
		}
	})

	step(t, "concurrent PUT creates one immutable path exactly once", func(t *testing.T) {
		content := []byte("concurrent-linux-arm64-" + runID)
		checksum := sha256Hex(content)
		start := make(chan struct{})
		ready := make(chan struct{}, 2)
		results := make(chan apiResponse, 2)
		errors := make(chan error, 2)
		var wait sync.WaitGroup
		for range 2 {
			wait.Add(1)
			go func() {
				defer wait.Done()
				ready <- struct{}{}
				<-start
				response, requestErr := client.do(
					ctx, http.MethodPut, artifactPath(repoA, concurrentPath), publisherToken.Secret, content,
					map[string]string{"Content-Type": "application/octet-stream", "X-Checksum-Sha256": checksum},
				)
				if requestErr != nil {
					errors <- requestErr
					return
				}
				results <- response
			}()
		}
		<-ready
		<-ready
		close(start)
		wait.Wait()
		close(results)
		close(errors)
		for requestErr := range errors {
			t.Fatal(requestErr)
		}
		responses := make([]apiResponse, 0, 2)
		for response := range results {
			responses = append(responses, response)
		}
		if len(responses) != 2 {
			t.Fatalf("concurrent PUT responses = %d, want 2", len(responses))
		}
		sort.Slice(responses, func(left, right int) bool { return responses[left].Status < responses[right].Status })
		if responses[0].Status != http.StatusCreated {
			t.Fatalf("first concurrent PUT status = %d, want 201; body=%s", responses[0].Status, responses[0].Body)
		}
		requireProblem(t, responses[1], http.StatusConflict, "conflict")
	})

	step(t, "artifact overwrite is rejected and foreign artifact is isolated", func(t *testing.T) {
		overwrite := mustRequest(
			t, ctx, client, http.MethodPut, artifactPath(repoA, artifactOnePath), publisherToken.Secret,
			artifactOneContent,
			map[string]string{"Content-Type": "application/octet-stream", "X-Checksum-Sha256": artifactOneSHA},
		)
		requireProblem(t, overwrite, http.StatusConflict, "conflict")
		foreignContent := []byte("foreign-repository-only-" + runID)
		foreign := mustRequest(
			t, ctx, client, http.MethodPut, artifactPath(repoB, foreignPath), publisherToken.Secret,
			foreignContent,
			map[string]string{"Content-Type": "application/octet-stream", "X-Checksum-Sha256": sha256Hex(foreignContent)},
		)
		requireStatus(t, foreign, http.StatusCreated)
	})

	releaseOnePath := releasePath(repoA, packageName, versionOne)
	releaseTwoPath := releasePath(repoA, packageName, versionTwo)

	step(t, "draft artifact removal is visible and coordinates are unique", func(t *testing.T) {
		created, _ := requestJSON[api.Release](
			t, ctx, client, http.MethodPost, releasesPath(repoA, packageName), publisherToken.Secret,
			api.CreateReleaseRequest{Version: versionOne}, keys.next("release-v1"), http.StatusCreated,
		)
		if created.State != "draft" {
			t.Fatalf("new release state = %q, want draft", created.State)
		}
		temporaryRole := api.OptionalCoordinate("temporary")
		removable, _ := requestJSON[api.ReleaseArtifact](
			t, ctx, client, http.MethodPost, releaseOnePath+"/artifacts", publisherToken.Secret,
			api.AddReleaseArtifactRequest{ArtifactPath: artifactTwoPath, Os: "linux", Arch: "arm64", Role: &temporaryRole},
			keys.next("add-removable"), http.StatusCreated,
		)
		remove := mustRequest(
			t, ctx, client, http.MethodDelete, releaseOnePath+"/artifacts/"+removable.Id.String(), publisherToken.Secret,
			nil, mutationHeaders(keys.next("remove-removable")),
		)
		requireStatus(t, remove, http.StatusNoContent)
		current, _ := requestJSONNoBody[api.Release](t, ctx, client, http.MethodGet, releaseOnePath, publisherToken.Secret, http.StatusOK)
		if len(current.Artifacts) != 0 {
			t.Fatalf("draft artifacts after removal = %+v, want empty", current.Artifacts)
		}
		releaseArtifact, _ = requestJSON[api.ReleaseArtifact](
			t, ctx, client, http.MethodPost, releaseOnePath+"/artifacts", publisherToken.Secret,
			api.AddReleaseArtifactRequest{ArtifactPath: artifactOnePath, Os: "linux", Arch: "arm64"},
			keys.next("add-v1-artifact"), http.StatusCreated,
		)
		duplicate := mustRequest(
			t, ctx, client, http.MethodPost, releaseOnePath+"/artifacts", publisherToken.Secret,
			marshalJSON(t, api.AddReleaseArtifactRequest{ArtifactPath: artifactTwoPath, Os: "linux", Arch: "arm64"}),
			jsonMutationHeaders(keys.next("duplicate-coordinate")),
		)
		requireProblem(t, duplicate, http.StatusConflict, "conflict")
	})

	step(t, "cross-repository artifact reference is rejected", func(t *testing.T) {
		foreignRole := api.OptionalCoordinate("foreign")
		response := mustRequest(
			t, ctx, client, http.MethodPost, releaseOnePath+"/artifacts", publisherToken.Secret,
			marshalJSON(t, api.AddReleaseArtifactRequest{ArtifactPath: foreignPath, Os: "linux", Arch: "arm64", Role: &foreignRole}),
			jsonMutationHeaders(keys.next("cross-repository-artifact")),
		)
		requireProblem(t, response, http.StatusUnprocessableEntity, "cross-repository-artifact")
	})

	step(t, "draft cancellation releases the version coordinate", func(t *testing.T) {
		first, _ := requestJSON[api.Release](
			t, ctx, client, http.MethodPost, releasesPath(repoA, packageName), publisherToken.Secret,
			api.CreateReleaseRequest{Version: cancelVersion}, keys.next("create-cancel-release"), http.StatusCreated,
		)
		cancelResponse := mustRequest(
			t, ctx, client, http.MethodDelete, releasePath(repoA, packageName, cancelVersion), publisherToken.Secret,
			nil, mutationHeaders(keys.next("cancel-release")),
		)
		requireStatus(t, cancelResponse, http.StatusNoContent)
		second, _ := requestJSON[api.Release](
			t, ctx, client, http.MethodPost, releasesPath(repoA, packageName), publisherToken.Secret,
			api.CreateReleaseRequest{Version: cancelVersion}, keys.next("recreate-cancel-release"), http.StatusCreated,
		)
		if first.Id == second.Id {
			t.Fatalf("recreated release retained canceled release ID %s", first.Id)
		}
	})

	step(t, "publish two immutable signed releases", func(t *testing.T) {
		_, _ = requestJSON[api.Release](
			t, ctx, client, http.MethodPost, releasesPath(repoA, packageName), publisherToken.Secret,
			api.CreateReleaseRequest{Version: versionTwo}, keys.next("release-v2"), http.StatusCreated,
		)
		_, _ = requestJSON[api.ReleaseArtifact](
			t, ctx, client, http.MethodPost, releaseTwoPath+"/artifacts", publisherToken.Secret,
			api.AddReleaseArtifactRequest{ArtifactPath: artifactTwoPath, Os: "linux", Arch: "arm64"},
			keys.next("add-v2-artifact"), http.StatusCreated,
		)
		publishedOne, _ := requestJSONNoBodyWithKey[api.Release](
			t, ctx, client, http.MethodPost, releaseOnePath+"/publish", publisherToken.Secret,
			keys.next("publish-v1"), http.StatusOK,
		)
		publishedTwo, _ := requestJSONNoBodyWithKey[api.Release](
			t, ctx, client, http.MethodPost, releaseTwoPath+"/publish", publisherToken.Secret,
			keys.next("publish-v2"), http.StatusOK,
		)
		if publishedOne.State != "published" || publishedTwo.State != "published" {
			t.Fatalf("published states: v1=%q v2=%q", publishedOne.State, publishedTwo.State)
		}
		manifestOne, _ = requestJSONNoBody[api.ReleaseManifest](
			t, ctx, client, http.MethodGet, releaseOnePath+"/manifest", readerToken.Secret, http.StatusOK,
		)
	})

	step(t, "published release and artifact mutations are rejected", func(t *testing.T) {
		add := mustRequest(
			t, ctx, client, http.MethodPost, releaseOnePath+"/artifacts", publisherToken.Secret,
			marshalJSON(t, api.AddReleaseArtifactRequest{ArtifactPath: artifactTwoPath, Os: "linux", Arch: "amd64"}),
			jsonMutationHeaders(keys.next("post-publish-add")),
		)
		requireProblem(t, add, http.StatusConflict, "conflict")
		remove := mustRequest(
			t, ctx, client, http.MethodDelete, releaseOnePath+"/artifacts/"+releaseArtifact.Id.String(), publisherToken.Secret,
			nil, mutationHeaders(keys.next("post-publish-remove")),
		)
		requireProblem(t, remove, http.StatusConflict, "conflict")
		cancelPublished := mustRequest(
			t, ctx, client, http.MethodDelete, releaseOnePath, publisherToken.Secret,
			nil, mutationHeaders(keys.next("post-publish-cancel")),
		)
		requireProblem(t, cancelPublished, http.StatusConflict, "conflict")
		overwrite := mustRequest(
			t, ctx, client, http.MethodPut, artifactPath(repoA, artifactOnePath), publisherToken.Secret,
			artifactOneContent,
			map[string]string{"Content-Type": "application/octet-stream", "X-Checksum-Sha256": artifactOneSHA},
		)
		requireProblem(t, overwrite, http.StatusConflict, "conflict")
	})

	step(t, "verify canonical manifest SHA and Ed25519 signature independently", func(t *testing.T) {
		manifestOneBytes = decodeBase64URL(t, manifestOne.Manifest)
		manifestSignature = decodeBase64URL(t, manifestOne.Signature)
		if actual := sha256Hex(manifestOneBytes); actual != manifestOne.ManifestSha256 {
			t.Fatalf("manifest SHA = %s, response = %s", actual, manifestOne.ManifestSha256)
		}
		canonical, err := jcs.Transform(manifestOneBytes)
		if err != nil {
			t.Fatalf("canonicalize returned manifest: %v", err)
		}
		if !bytes.Equal(canonical, manifestOneBytes) {
			t.Fatalf("returned manifest bytes are not RFC 8785 canonical JSON")
		}
		if !ed25519.Verify(publicKey, manifestOneBytes, manifestSignature) {
			t.Fatalf("manifest signature does not verify with deployment public key")
		}
		fingerprint := sha256.Sum256(publicKey)
		if expected := "ed25519:" + hex.EncodeToString(fingerprint[:]); manifestOne.KeyId != expected {
			t.Fatalf("manifest key ID = %q, deployment key ID = %q", manifestOne.KeyId, expected)
		}
		var document manifestDocument
		decodeJSON(t, manifestOneBytes, &document)
		if document.SchemaVersion != 1 || document.Repository != repoA || document.Package != packageName || document.Version != versionOne || len(document.Artifacts) != 1 {
			t.Fatalf("manifest identity/content = %+v", document)
		}
		entry := document.Artifacts[0]
		if entry.Path != artifactOnePath || entry.OS != "linux" || entry.Arch != "arm64" || entry.SHA256 != artifactOneSHA || entry.Size != int64(len(artifactOneContent)) {
			t.Fatalf("manifest artifact = %+v", entry)
		}
	})

	step(t, "cross-package promotion is rejected", func(t *testing.T) {
		response := mustRequest(
			t, ctx, client, http.MethodPost, promotionPath(repoA, otherPackage, "candidate"), publisherToken.Secret,
			marshalJSON(t, api.PromoteChannelRequest{Version: versionOne, Reason: "must remain package scoped"}),
			jsonMutationHeaders(keys.next("cross-package-promotion")),
		)
		requireProblem(t, response, http.StatusUnprocessableEntity, "release-not-in-package")
	})

	step(t, "candidate promotion preserves ordered rollback history", func(t *testing.T) {
		promotions := []struct {
			version string
			reason  string
		}{
			{versionOne, "initial candidate"},
			{versionTwo, "upgrade candidate"},
			{versionOne, "rollback candidate"},
		}
		for index, promotion := range promotions {
			revision, _ := requestJSON[api.ChannelRevision](
				t, ctx, client, http.MethodPost, promotionPath(repoA, packageName, "candidate"), publisherToken.Secret,
				api.PromoteChannelRequest{Version: promotion.version, Reason: promotion.reason},
				keys.next("promote-"+strconv.Itoa(index)), http.StatusOK,
			)
			if revision.ToVersion != promotion.version || revision.Reason != promotion.reason {
				t.Fatalf("promotion revision = %+v", revision)
			}
		}
		channel, _ := requestJSONNoBody[api.Channel](
			t, ctx, client, http.MethodGet, channelPath(repoA, packageName, "candidate"), readerToken.Secret, http.StatusOK,
		)
		if channel.CurrentVersion == nil || *channel.CurrentVersion != versionOne {
			t.Fatalf("candidate current version = %v, want rollback %s", channel.CurrentVersion, versionOne)
		}
		history, _ := requestJSONNoBody[api.ChannelRevisionPage](
			t, ctx, client, http.MethodGet, channelPath(repoA, packageName, "candidate")+"/history?limit=10", readerToken.Secret, http.StatusOK,
		)
		if len(history.Items) != 3 {
			t.Fatalf("channel history length = %d, want 3", len(history.Items))
		}
		wantTargets := []string{versionOne, versionTwo, versionOne}
		wantFrom := []*string{&versionTwo, &versionOne, nil}
		seen := map[string]struct{}{}
		for index, revision := range history.Items {
			if revision.ToVersion != wantTargets[index] || !equalOptionalString(revision.FromVersion, wantFrom[index]) {
				t.Fatalf("history[%d] = %+v, want from=%v to=%s", index, revision, wantFrom[index], wantTargets[index])
			}
			if _, duplicate := seen[revision.Id.String()]; duplicate {
				t.Fatalf("channel history repeats revision ID %s", revision.Id)
			}
			seen[revision.Id.String()] = struct{}{}
		}
	})

	step(t, "reader resolves and proxy-downloads exact bytes", func(t *testing.T) {
		resolved, _ := requestJSONNoBody[api.ResolveResponse](
			t, ctx, client, http.MethodGet, resolvePath(repoA, packageName, "candidate", false), readerToken.Secret, http.StatusOK,
		)
		assertResolution(t, resolved, versionOne, artifactOnePath, artifactOneSHA, int64(len(artifactOneContent)), manifestOneBytes, manifestSignature)
		if parsed, err := url.Parse(resolved.DownloadUrl); err != nil || parsed.IsAbs() || !strings.Contains(parsed.RawQuery, "redirect=false") {
			t.Fatalf("proxy download URL = %q, want relative API URL with redirect=false", resolved.DownloadUrl)
		}
		proxy := mustRequest(t, ctx, client, http.MethodGet, resolved.DownloadUrl, readerToken.Secret, nil, nil)
		requireStatus(t, proxy, http.StatusOK)
		assertDownloadedBytes(t, proxy.Body, artifactOneContent, resolved.Artifact.Sha256, resolved.Artifact.Size)
	})

	step(t, "public redirect host downloads without repository credentials", func(t *testing.T) {
		resolved, _ := requestJSONNoBody[api.ResolveResponse](
			t, ctx, client, http.MethodGet, resolvePath(repoA, packageName, "candidate", true), readerToken.Secret, http.StatusOK,
		)
		assertPublicDownload(t, ctx, resolved.DownloadUrl, publicEndpoint, artifactOneContent, artifactOneSHA, int64(len(artifactOneContent)))

		redirect := mustRequest(t, ctx, client, http.MethodGet, artifactPath(repoA, artifactOnePath), readerToken.Secret, nil, nil)
		requireStatus(t, redirect, http.StatusTemporaryRedirect)
		location := redirect.Header.Get("Location")
		if location == "" {
			t.Fatalf("artifact redirect omitted Location")
		}
		assertPublicDownload(t, ctx, location, publicEndpoint, artifactOneContent, artifactOneSHA, int64(len(artifactOneContent)))
	})

	step(t, "API and Worker restart retains resolvable state", func(t *testing.T) {
		if _, err := composeOutput(ctx, project, "restart", "api", "worker"); err != nil {
			t.Fatal(err)
		}
		if err := waitForReady(ctx, baseURL, 60*time.Second); err != nil {
			t.Fatal(err)
		}
		if err := waitForReady(ctx, workerURL, 60*time.Second); err != nil {
			t.Fatal(err)
		}
		resolved, _ := requestJSONNoBody[api.ResolveResponse](
			t, ctx, client, http.MethodGet, resolvePath(repoA, packageName, "candidate", false), readerToken.Secret, http.StatusOK,
		)
		assertResolution(t, resolved, versionOne, artifactOnePath, artifactOneSHA, int64(len(artifactOneContent)), manifestOneBytes, manifestSignature)
	})

	step(t, "audit listing contains sanitized denial evidence", func(t *testing.T) {
		page, response := requestJSONNoBody[api.AuditEventPage](
			t, ctx, client, http.MethodGet, "/api/v1/audit-events?limit=200", adminToken, http.StatusOK,
		)
		if len(page.Items) == 0 {
			t.Fatalf("audit listing is empty")
		}
		for _, secret := range []string{adminToken, publisherToken.Secret, readerToken.Secret, emptyToken.Secret} {
			if strings.Contains(string(response.Body), secret) {
				t.Fatalf("audit response contains a bearer secret")
			}
		}
		var denial *api.AuditEvent
		var forbidden *api.AuditEvent
		var invalidToken *api.AuditEvent
		var hiddenRepository *api.AuditEvent
		for index := range page.Items {
			event := &page.Items[index]
			if event.Outcome == "denied" && event.Code != nil && *event.Code == "cross-repository-artifact" &&
				event.ActorId != nil && *event.ActorId == publisherToken.Id {
				denial = event
			}
			if event.Outcome == "denied" && event.Code != nil && *event.Code == "forbidden" &&
				event.ActorId != nil && *event.ActorId == readerToken.Id {
				forbidden = event
			}
			if event.Outcome == "denied" && event.Code != nil && *event.Code == "invalid-token" && event.ActorId == nil {
				invalidToken = event
			}
			if event.Outcome == "denied" && event.Code != nil && *event.Code == "repository-not-allowed" &&
				event.ActorId != nil && *event.ActorId == emptyToken.Id {
				hiddenRepository = event
			}
		}
		if denial == nil {
			t.Fatalf("audit listing has no cross-repository denial for publisher token %s", publisherToken.Id)
		}
		encoded := string(marshalJSON(t, denial))
		if strings.Contains(encoded, foreignPath) || strings.Contains(encoded, publisherToken.Secret) || strings.Contains(encoded, string(artifactOneContent)) {
			t.Fatalf("denial AuditEvent contains a path, bearer, or artifact payload")
		}
		if denial.Action == "" || denial.ResourceType == "" || denial.RequestId == "" || denial.RepositoryId == nil || *denial.RepositoryId != repositoryA.Id {
			t.Fatalf("denial AuditEvent lacks safe attribution: %+v", denial)
		}
		if forbidden == nil {
			t.Fatalf("audit listing has no forbidden event for reader token %s", readerToken.Id)
		}
		if invalidToken == nil {
			t.Fatalf("audit listing has no invalid-token event without an actor")
		}
		if hiddenRepository == nil {
			t.Fatalf("audit listing has no hidden repository denial for empty-allowlist token %s", emptyToken.Id)
		}
	})

	step(t, "per-token read burst returns bounded rate-limit problems", func(t *testing.T) {
		const requestCount = 250
		start := make(chan struct{})
		results := make(chan apiResponse, requestCount)
		errors := make(chan error, requestCount)
		var wait sync.WaitGroup
		for range requestCount {
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				response, requestErr := client.do(
					ctx, http.MethodGet, "/api/v1/repositories/"+repoA, readerToken.Secret, nil, nil,
				)
				if requestErr != nil {
					errors <- requestErr
					return
				}
				results <- response
			}()
		}
		close(start)
		wait.Wait()
		close(results)
		close(errors)
		for requestErr := range errors {
			t.Fatal(requestErr)
		}
		rateLimited := 0
		for response := range results {
			switch response.Status {
			case http.StatusOK:
			case http.StatusTooManyRequests:
				rateLimited++
				requireProblem(t, response, http.StatusTooManyRequests, "rate-limit-exceeded")
				retryAfter, err := strconv.Atoi(response.Header.Get("Retry-After"))
				if err != nil || retryAfter < 1 {
					t.Fatalf("rate-limit Retry-After = %q", response.Header.Get("Retry-After"))
				}
			default:
				t.Fatalf("burst read status = %d; body=%s", response.Status, response.Body)
			}
		}
		if rateLimited == 0 {
			t.Fatalf("%d concurrent reads produced no 429 response", requestCount)
		}
	})

	step(t, "rate-limit denials are audited", func(t *testing.T) {
		page, response := requestJSONNoBody[api.AuditEventPage](
			t, ctx, client, http.MethodGet, "/api/v1/audit-events?limit=200", adminToken, http.StatusOK,
		)
		if strings.Contains(string(response.Body), readerToken.Secret) {
			t.Fatalf("rate-limit audit response contains reader bearer secret")
		}
		for _, event := range page.Items {
			if event.Outcome == "denied" && event.Code != nil && *event.Code == "rate-limit-exceeded" &&
				event.ActorId != nil && *event.ActorId == readerToken.Id {
				return
			}
		}
		t.Fatalf("audit listing has no rate-limit denial for reader token %s", readerToken.Id)
	})
}

type idempotencyKeys struct {
	prefix string
	nextID int
}

func (keys *idempotencyKeys) next(label string) string {
	keys.nextID++
	return fmt.Sprintf("%s-%03d-%s", keys.prefix, keys.nextID, label)
}

type manifestDocument struct {
	SchemaVersion int                `json:"schemaVersion"`
	Repository    string             `json:"repository"`
	Package       string             `json:"package"`
	Version       string             `json:"version"`
	PublishedAt   time.Time          `json:"publishedAt"`
	Artifacts     []manifestArtifact `json:"artifacts"`
}

type manifestArtifact struct {
	Path      string `json:"path"`
	Filename  string `json:"filename"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	Variant   string `json:"variant"`
	Role      string `json:"role"`
	MediaType string `json:"mediaType"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
}

func step(t *testing.T, name string, test func(*testing.T)) {
	t.Helper()
	if !t.Run(name, test) {
		t.FailNow()
	}
}

func uniqueRunID(t *testing.T) string {
	t.Helper()
	random := make([]byte, 4)
	if _, err := cryptorand.Read(random); err != nil {
		t.Fatalf("generate E2E run ID: %v", err)
	}
	return "e2e-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + hex.EncodeToString(random)
}

func environmentOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func createServiceAccount(t *testing.T, ctx context.Context, client *apiClient, token, name, key string) api.ServiceAccount {
	t.Helper()
	account, _ := requestJSON[api.ServiceAccount](
		t, ctx, client, http.MethodPost, "/api/v1/service-accounts", token,
		api.CreateServiceAccountRequest{Name: name}, key, http.StatusCreated,
	)
	return account
}

func requestJSON[T any](
	t *testing.T,
	ctx context.Context,
	client *apiClient,
	method, path, token string,
	body any,
	idempotencyKey string,
	wantStatus int,
) (T, apiResponse) {
	t.Helper()
	return requestJSONBytes[T](t, ctx, client, method, path, token, marshalJSON(t, body), idempotencyKey, wantStatus)
}

func requestJSONBytes[T any](
	t *testing.T,
	ctx context.Context,
	client *apiClient,
	method, path, token string,
	body []byte,
	idempotencyKey string,
	wantStatus int,
) (T, apiResponse) {
	t.Helper()
	response := mustRequest(t, ctx, client, method, path, token, body, jsonMutationHeaders(idempotencyKey))
	requireStatus(t, response, wantStatus)
	var value T
	decodeJSON(t, response.Body, &value)
	return value, response
}

func requestJSONNoBody[T any](
	t *testing.T,
	ctx context.Context,
	client *apiClient,
	method, path, token string,
	wantStatus int,
) (T, apiResponse) {
	t.Helper()
	response := mustRequest(t, ctx, client, method, path, token, nil, nil)
	requireStatus(t, response, wantStatus)
	var value T
	decodeJSON(t, response.Body, &value)
	return value, response
}

func requestJSONNoBodyWithKey[T any](
	t *testing.T,
	ctx context.Context,
	client *apiClient,
	method, path, token, idempotencyKey string,
	wantStatus int,
) (T, apiResponse) {
	t.Helper()
	response := mustRequest(t, ctx, client, method, path, token, nil, mutationHeaders(idempotencyKey))
	requireStatus(t, response, wantStatus)
	var value T
	decodeJSON(t, response.Body, &value)
	return value, response
}

func mustRequest(
	t *testing.T,
	ctx context.Context,
	client *apiClient,
	method, path, token string,
	body []byte,
	headers map[string]string,
) apiResponse {
	t.Helper()
	for attempt := 0; attempt < 4; attempt++ {
		response, err := client.do(ctx, method, path, token, body, headers)
		if err != nil {
			t.Fatal(err)
		}
		if response.Status != http.StatusTooManyRequests || attempt == 3 {
			return response
		}
		retryAfter, err := strconv.Atoi(response.Header.Get("Retry-After"))
		if err != nil || retryAfter < 1 {
			t.Fatalf("rate-limit Retry-After = %q", response.Header.Get("Retry-After"))
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Duration(retryAfter) * time.Second):
		}
	}
	t.Fatalf("request retry loop ended unexpectedly")
	return apiResponse{}
}

func requireStatus(t *testing.T, response apiResponse, want int) {
	t.Helper()
	if response.Status != want {
		t.Fatalf("HTTP status = %d, want %d; body=%s", response.Status, want, response.Body)
	}
}

func requireProblem(t *testing.T, response apiResponse, status int, code string) api.Problem {
	t.Helper()
	requireStatus(t, response, status)
	if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "application/problem+json") {
		t.Fatalf("problem Content-Type = %q", contentType)
	}
	var problem api.Problem
	decodeJSON(t, response.Body, &problem)
	if problem.Status != status || problem.Code != code || problem.Type == "" || problem.Title == "" || problem.RequestId == "" {
		t.Fatalf("problem = %+v, want status=%d code=%q and required RFC 9457 fields", problem, status, code)
	}
	return problem
}

func marshalJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode JSON: %v", err)
	}
	return encoded
}

func decodeJSON(t *testing.T, encoded []byte, destination any) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		t.Fatalf("decode JSON %q: %v", encoded, err)
	}
}

func mutationHeaders(key string) map[string]string {
	return map[string]string{"Idempotency-Key": key}
}

func jsonMutationHeaders(key string) map[string]string {
	return map[string]string{"Content-Type": "application/json", "Idempotency-Key": key}
}

func artifactPath(repository, path string) string {
	return "/api/v1/repositories/" + repository + "/artifacts/" + path
}

func releasesPath(repository, packageName string) string {
	return "/api/v1/repositories/" + repository + "/packages/" + packageName + "/releases"
}

func releasePath(repository, packageName, version string) string {
	return releasesPath(repository, packageName) + "/" + version
}

func channelPath(repository, packageName, channel string) string {
	return "/api/v1/repositories/" + repository + "/packages/" + packageName + "/channels/" + channel
}

func promotionPath(repository, packageName, channel string) string {
	return channelPath(repository, packageName, channel) + "/promotions"
}

func resolvePath(repository, packageName, channel string, redirect bool) string {
	return channelPath(repository, packageName, channel) + "/resolve?os=linux&arch=arm64&redirect=" + strconv.FormatBool(redirect)
}

func sha256Hex(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

func decodeBase64URL(t *testing.T, encoded string) []byte {
	t.Helper()
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode base64url value: %v", err)
	}
	return decoded
}

func equalOptionalString(actual, expected *string) bool {
	if actual == nil || expected == nil {
		return actual == nil && expected == nil
	}
	return *actual == *expected
}

func assertResolution(
	t *testing.T,
	resolved api.ResolveResponse,
	version, path, checksum string,
	size int64,
	manifest, signature []byte,
) {
	t.Helper()
	if resolved.Version != version || resolved.Artifact.Path != path || resolved.Artifact.Os != "linux" || resolved.Artifact.Arch != "arm64" ||
		resolved.Artifact.Variant != "" || resolved.Artifact.Role != "" || resolved.Artifact.Sha256 != checksum || resolved.Artifact.Size != size {
		t.Fatalf("resolution metadata = %+v", resolved)
	}
	if !bytes.Equal(decodeBase64URL(t, resolved.Manifest), manifest) || !bytes.Equal(decodeBase64URL(t, resolved.Signature), signature) {
		t.Fatalf("resolution signed manifest differs from published manifest")
	}
}

func assertDownloadedBytes(t *testing.T, actual, expected []byte, checksum string, size int64) {
	t.Helper()
	if !bytes.Equal(actual, expected) || int64(len(actual)) != size || sha256Hex(actual) != checksum {
		t.Fatalf("downloaded bytes size/SHA/content mismatch: size=%d SHA=%s", len(actual), sha256Hex(actual))
	}
}

func assertPublicDownload(
	t *testing.T,
	ctx context.Context,
	downloadURL, publicEndpoint string,
	expected []byte,
	checksum string,
	size int64,
) {
	t.Helper()
	parsedDownload, err := url.Parse(downloadURL)
	if err != nil || !parsedDownload.IsAbs() {
		t.Fatalf("public download URL = %q: %v", downloadURL, err)
	}
	parsedEndpoint, err := url.Parse(publicEndpoint)
	if err != nil {
		t.Fatalf("parse expected public endpoint: %v", err)
	}
	if parsedDownload.Scheme != parsedEndpoint.Scheme || parsedDownload.Host != parsedEndpoint.Host || parsedDownload.User != nil {
		t.Fatalf("public download origin/userinfo = %s://%s %v, want %s://%s without userinfo", parsedDownload.Scheme, parsedDownload.Host, parsedDownload.User, parsedEndpoint.Scheme, parsedEndpoint.Host)
	}
	downloaded, err := downloadWithoutCredentials(ctx, downloadURL)
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, downloaded, http.StatusOK)
	assertDownloadedBytes(t, downloaded.Body, expected, checksum, size)
}
