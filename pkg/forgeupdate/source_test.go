package forgeupdate

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

const sourceTestInstallKey = "11111111-2222-4333-8444-555555555555"

type sourceFixture struct {
	signed           SignedManifest
	artifact         ResolvedArtifact
	payload          []byte
	downloadURL      string
	resolveRedirect  string
	downloadRedirect string
	headerOverrides  map[string]string
	resolveCalls     atomic.Int32
	downloadCalls    atomic.Int32
}

func (fixture *sourceFixture) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" {
		http.Error(response, "credentials must not be sent", http.StatusBadRequest)
		return
	}
	if request.URL.Query().Get("os") != "linux" || request.URL.Query().Get("arch") != "amd64" ||
		request.URL.Query().Get("variant") != "" {
		http.Error(response, "wrong platform query", http.StatusBadRequest)
		return
	}
	switch request.URL.Path {
	case "/i/" + sourceTestInstallKey + "/edgecli/resolve":
		fixture.resolveCalls.Add(1)
		if fixture.resolveRedirect != "" {
			http.Redirect(response, request, fixture.resolveRedirect, http.StatusFound)
			return
		}
		downloadURL := fixture.downloadURL
		if downloadURL == "" {
			downloadURL = "/i/" + sourceTestInstallKey + "/edgecli/download?arch=amd64&os=linux" +
				"&sha256=" + fixture.artifact.SHA256 + "&version=2.0.0"
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(map[string]any{
			"version":   "2.0.0",
			"manifest":  base64.RawURLEncoding.EncodeToString(fixture.signed.Manifest),
			"keyId":     fixture.signed.KeyID,
			"signature": base64.RawURLEncoding.EncodeToString(fixture.signed.Signature),
			"artifact": map[string]any{
				"path": fixture.artifact.Path, "os": fixture.artifact.OS,
				"arch": fixture.artifact.Arch, "variant": fixture.artifact.Variant,
				"role": fixture.artifact.Role, "sha256": fixture.artifact.SHA256,
				"size": fixture.artifact.Size,
			},
			"downloadUrl": downloadURL,
		})
	case "/i/" + sourceTestInstallKey + "/edgecli/download":
		if request.URL.Query().Get("version") != "2.0.0" ||
			request.URL.Query().Get("sha256") != fixture.artifact.SHA256 {
			http.Error(response, "download is not version and digest pinned", http.StatusBadRequest)
			return
		}
		fixture.downloadCalls.Add(1)
		if fixture.downloadRedirect != "" {
			http.Redirect(response, request, fixture.downloadRedirect, http.StatusFound)
			return
		}
		headers := map[string]string{
			"X-Checksum-Sha256":        fixture.artifact.SHA256,
			"X-Forge-Version":          "2.0.0",
			"X-Forge-Install-Strategy": "",
			"X-Forge-Install-Format":   "",
		}
		for name, value := range fixture.headerOverrides {
			headers[name] = value
		}
		for name, value := range headers {
			response.Header().Set(name, value)
		}
		_, _ = response.Write(fixture.payload)
	default:
		http.NotFound(response, request)
	}
}

func TestClientCheckThenApplySelfBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows self-replacement deliberately requires an external helper")
	}
	payload := []byte("new private CLI")
	signed, verifier, artifact := sourceReleaseForTest(
		t, payload, InstallStrategySelfReplace, ArchiveRaw, "",
	)
	fixture := &sourceFixture{
		signed: signed, artifact: resolvedArtifactForTest(artifact), payload: payload,
		headerOverrides: map[string]string{
			"X-Forge-Install-Strategy": string(artifact.Strategy),
			"X-Forge-Install-Format":   string(artifact.Format),
		},
	}
	server := httptest.NewServer(fixture)
	defer server.Close()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	jar.SetCookies(serverURL, []*http.Cookie{{Name: "session", Value: "must-not-be-sent"}})
	source, err := NewHTTPSource(HTTPSourceConfig{
		BaseURL: server.URL, Product: "edgecli", InstallKey: sourceTestInstallKey,
		HTTPClient: &http.Client{Jar: jar},
	})
	if err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(t.TempDir(), "edgecli")
	if err := os.WriteFile(target, []byte("old CLI"), 0o700); err != nil {
		t.Fatal(err)
	}
	var probes atomic.Int32
	client, err := NewClient(ClientConfig{
		Source: source, Verifier: verifier,
		SelfBinary: SelfBinaryOptions{
			Target: target,
			Probe: func(_ context.Context, candidate Candidate) error {
				probes.Add(1)
				assertFileContent(t, candidate.Path, payload)
				if candidate.Version != "2.0.0" ||
					candidate.Artifact.Strategy != InstallStrategySelfReplace {
					t.Fatalf("candidate = %+v", candidate)
				}
				return nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	selection := sourceSelectionForTest("1.0.0")
	plan, err := client.Check(t.Context(), selection)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.resolveCalls.Load() != 1 || fixture.downloadCalls.Load() != 0 {
		t.Fatalf("after Check resolve=%d download=%d",
			fixture.resolveCalls.Load(), fixture.downloadCalls.Load())
	}
	if plan.Version() != "2.0.0" || plan.Artifact().Kind != ArtifactBinary {
		t.Fatalf("plan version=%q artifact=%+v", plan.Version(), plan.Artifact())
	}

	anotherClient, err := NewClient(ClientConfig{Source: source, Verifier: verifier})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := anotherClient.Apply(t.Context(), plan); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("foreign Client Apply() error = %v", err)
	}

	result, err := client.Apply(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.Kind() != ArtifactBinary || result.SelfBinary() == nil || result.Bundle() != nil {
		t.Fatalf("ApplyResult kind=%q self=%v bundle=%v",
			result.Kind(), result.SelfBinary(), result.Bundle())
	}
	if probes.Load() != 1 || fixture.downloadCalls.Load() != 1 {
		t.Fatalf("probes=%d downloads=%d", probes.Load(), fixture.downloadCalls.Load())
	}
	assertFileContent(t, target, payload)
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("installed mode = %o, want signed mode 755", info.Mode().Perm())
	}
	if err := result.SelfBinary().Finalize(); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Apply(t.Context(), plan); !errors.Is(err, ErrPlanUsed) {
		t.Fatalf("second Apply() error = %v", err)
	}
	if fixture.downloadCalls.Load() != 1 {
		t.Fatalf("second Apply downloaded again: %d", fixture.downloadCalls.Load())
	}
}

func TestClientApplyDispatchesBundle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows bundle activation requires an external helper")
	}
	entries := []testArchiveEntry{
		{name: "bin/edgecli", content: []byte("bundled CLI"), mode: 0o755},
		{name: "share/config.json", content: []byte("{}"), mode: 0o644},
	}
	payload := buildArchive(t, ArchiveTarGz, entries)
	signed, verifier, artifact := sourceReleaseForTest(
		t, payload, InstallStrategyBundle, ArchiveTarGz, "bin/edgecli",
	)
	fixture := &sourceFixture{
		signed: signed, artifact: resolvedArtifactForTest(artifact), payload: payload,
		headerOverrides: map[string]string{
			"X-Forge-Install-Strategy": string(artifact.Strategy),
			"X-Forge-Install-Format":   string(artifact.Format),
		},
	}
	server := httptest.NewServer(fixture)
	defer server.Close()
	source := newSourceForTest(t, server.URL)
	root := t.TempDir()
	client, err := NewClient(ClientConfig{
		Source: source, Verifier: verifier, Bundle: BundleOptions{Root: root},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := client.Check(t.Context(), sourceSelectionForTest("1.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Apply(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.Kind() != ArtifactBundle || result.Bundle() == nil || result.SelfBinary() != nil {
		t.Fatalf("ApplyResult kind=%q self=%v bundle=%v",
			result.Kind(), result.SelfBinary(), result.Bundle())
	}
	assertFileContent(t, result.Bundle().Entrypoint(), []byte("bundled CLI"))
	assertFileContent(t, result.Bundle().ActiveEntrypoint(), []byte("bundled CLI"))
	assertCurrentLink(t, root, "versions/2.0.0")
	if err := result.Bundle().Finalize(); err != nil {
		t.Fatal(err)
	}
}

func TestClientCheckNoUpdateDoesNotDownload(t *testing.T) {
	payload := []byte("same CLI")
	signed, verifier, artifact := sourceReleaseForTest(
		t, payload, InstallStrategySelfReplace, ArchiveRaw, "",
	)
	fixture := &sourceFixture{
		signed: signed, artifact: resolvedArtifactForTest(artifact), payload: payload,
	}
	server := httptest.NewServer(fixture)
	defer server.Close()
	client, err := NewClient(ClientConfig{
		Source: newSourceForTest(t, server.URL), Verifier: verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Check(t.Context(), sourceSelectionForTest("2.0.0")); !errors.Is(err, ErrNoUpdate) {
		t.Fatalf("Check() error = %v", err)
	}
	if fixture.downloadCalls.Load() != 0 {
		t.Fatalf("no-update check downloaded %d artifacts", fixture.downloadCalls.Load())
	}
}

func TestHTTPSourceRejectsUntrustedDownloadReferences(t *testing.T) {
	payload := []byte("CLI")
	signed, _, artifact := sourceReleaseForTest(
		t, payload, InstallStrategySelfReplace, ArchiveRaw, "",
	)
	basePath := "/i/" + sourceTestInstallKey + "/edgecli/download"
	digest := artifact.SHA256
	query := "?arch=amd64&os=linux&sha256=" + digest + "&version=2.0.0"
	tests := []struct {
		name string
		url  string
	}{
		{name: "absolute", url: "https://attacker.example" + basePath + query},
		{name: "network path", url: "//attacker.example" + basePath + query},
		{name: "relative", url: strings.TrimPrefix(basePath, "/") + query},
		{name: "wrong path", url: "/i/" + sourceTestInstallKey + "/other/download" + query},
		{name: "encoded slash", url: "/i/" + sourceTestInstallKey + "%2Fother/edgecli/download" + query},
		{name: "duplicate query", url: basePath + "?arch=amd64&arch=arm64&os=linux&sha256=" + digest + "&version=2.0.0"},
		{name: "extra query", url: basePath + query + "&channel=stable"},
		{name: "missing query", url: basePath + "?os=linux"},
		{name: "missing version", url: basePath + "?arch=amd64&os=linux&sha256=" + digest},
		{name: "wrong version", url: basePath + "?arch=amd64&os=linux&sha256=" + digest + "&version=2.0.1"},
		{name: "missing digest", url: basePath + "?arch=amd64&os=linux&version=2.0.0"},
		{name: "wrong digest", url: basePath + "?arch=amd64&os=linux&sha256=" + strings.Repeat("b", 64) + "&version=2.0.0"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := &sourceFixture{
				signed: signed, artifact: resolvedArtifactForTest(artifact),
				payload: payload, downloadURL: test.url,
			}
			server := httptest.NewServer(fixture)
			defer server.Close()
			source := newSourceForTest(t, server.URL)
			if _, err := source.Resolve(t.Context(), sourceSelectionForTest("1.0.0")); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("Resolve() error = %v", err)
			}
			if fixture.downloadCalls.Load() != 0 {
				t.Fatalf("Resolve() opened download URL")
			}
		})
	}
}

func TestHTTPSourceNeverFollowsResolveOrDownloadRedirects(t *testing.T) {
	var attackerCalls atomic.Int32
	attacker := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		attackerCalls.Add(1)
	}))
	defer attacker.Close()

	payload := []byte("CLI")
	signed, verifier, artifact := sourceReleaseForTest(
		t, payload, InstallStrategySelfReplace, ArchiveRaw, "",
	)
	t.Run("resolve", func(t *testing.T) {
		fixture := &sourceFixture{
			signed: signed, artifact: resolvedArtifactForTest(artifact),
			resolveRedirect: attacker.URL,
		}
		server := httptest.NewServer(fixture)
		defer server.Close()
		source := newSourceForTest(t, server.URL)
		_, err := source.Resolve(t.Context(), sourceSelectionForTest("1.0.0"))
		if !errors.Is(err, ErrHTTPStatus) {
			t.Fatalf("Resolve() error = %v", err)
		}
		if strings.Contains(err.Error(), sourceTestInstallKey) {
			t.Fatalf("error leaks install key: %v", err)
		}
	})
	t.Run("download", func(t *testing.T) {
		fixture := &sourceFixture{
			signed: signed, artifact: resolvedArtifactForTest(artifact),
			downloadRedirect: attacker.URL,
		}
		server := httptest.NewServer(fixture)
		defer server.Close()
		source := newSourceForTest(t, server.URL)
		resolution, err := source.Resolve(t.Context(), sourceSelectionForTest("1.0.0"))
		if err != nil {
			t.Fatal(err)
		}
		release, err := verifier.Verify(resolution.SignedManifest(), sourceSelectionForTest("1.0.0"))
		if err != nil {
			t.Fatal(err)
		}
		_, err = source.Open(t.Context(), resolution, release)
		if !errors.Is(err, ErrHTTPStatus) {
			t.Fatalf("Open() error = %v", err)
		}
		if strings.Contains(err.Error(), sourceTestInstallKey) {
			t.Fatalf("error leaks install key: %v", err)
		}
	})
	if attackerCalls.Load() != 0 {
		t.Fatalf("redirect target received %d requests", attackerCalls.Load())
	}
}

func TestClientRejectsUnsignedResolveMismatchAndDownloadHeaderMismatch(t *testing.T) {
	payload := []byte("new CLI")
	signed, verifier, artifact := sourceReleaseForTest(
		t, payload, InstallStrategySelfReplace, ArchiveRaw, "",
	)
	t.Run("resolve metadata", func(t *testing.T) {
		resolved := resolvedArtifactForTest(artifact)
		resolved.SHA256 = strings.Repeat("b", 64)
		fixture := &sourceFixture{signed: signed, artifact: resolved, payload: payload}
		server := httptest.NewServer(fixture)
		defer server.Close()
		client, err := NewClient(ClientConfig{
			Source: newSourceForTest(t, server.URL), Verifier: verifier,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Check(t.Context(), sourceSelectionForTest("1.0.0")); !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("Check() error = %v", err)
		}
		if fixture.downloadCalls.Load() != 0 {
			t.Fatalf("mismatched Check downloaded artifact")
		}
	})
	t.Run("download headers", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows self-replacement deliberately requires an external helper")
		}
		fixture := &sourceFixture{
			signed: signed, artifact: resolvedArtifactForTest(artifact), payload: payload,
			headerOverrides: map[string]string{
				"X-Checksum-Sha256":        artifact.SHA256,
				"X-Forge-Version":          "9.9.9",
				"X-Forge-Install-Strategy": string(artifact.Strategy),
				"X-Forge-Install-Format":   string(artifact.Format),
			},
		}
		server := httptest.NewServer(fixture)
		defer server.Close()
		target := filepath.Join(t.TempDir(), "edgecli")
		if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
			t.Fatal(err)
		}
		client, err := NewClient(ClientConfig{
			Source: newSourceForTest(t, server.URL), Verifier: verifier,
			SelfBinary: SelfBinaryOptions{
				Target: target, Probe: func(context.Context, Candidate) error { return nil },
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		plan, err := client.Check(t.Context(), sourceSelectionForTest("1.0.0"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Apply(t.Context(), plan); !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("Apply() error = %v", err)
		}
		assertFileContent(t, target, []byte("old"))
	})
}

func TestHTTPSourceTransportErrorRedactsInstallKey(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	baseURL := server.URL
	server.Close()
	source := newSourceForTest(t, baseURL)
	_, err := source.Resolve(t.Context(), sourceSelectionForTest("1.0.0"))
	if !errors.Is(err, ErrSource) {
		t.Fatalf("Resolve() error = %v", err)
	}
	if strings.Contains(err.Error(), sourceTestInstallKey) || strings.Contains(err.Error(), baseURL) {
		t.Fatalf("transport error leaks source URL or install key: %v", err)
	}
}

func sourceReleaseForTest(
	t *testing.T,
	payload []byte,
	strategy InstallStrategy,
	format ArchiveFormat,
	entrypoint string,
) (SignedManifest, Verifier, Artifact) {
	t.Helper()
	artifact := artifactForBytes(payload)
	artifactPath := "linux/amd64/edgecli"
	filename := "edgecli"
	mediaType := "application/octet-stream"
	if strategy == InstallStrategyBundle {
		artifactPath = "linux/amd64/edgecli." + string(format)
		filename = "edgecli." + string(format)
		mediaType = "application/gzip"
	}
	install := map[string]any{
		"strategy": strategy,
		"format":   format,
		"mode":     "0755",
	}
	if entrypoint != "" {
		install["entrypoint"] = entrypoint
	}
	document := map[string]any{
		"schemaVersion": 2,
		"repository":    "cli-releases",
		"package":       "edgecli",
		"product":       map[string]any{"slug": "edgecli", "command": "EdgeCLI"},
		"version":       "2.0.0",
		"publishedAt":   "2026-07-23T00:00:00Z",
		"artifacts": []any{map[string]any{
			"path": artifactPath, "filename": filename,
			"os": "linux", "arch": "amd64", "variant": "", "role": "binary",
			"mediaType": mediaType, "sha256": artifact.SHA256, "size": artifact.Size,
			"install": install,
		}},
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	signed, verifier := signForTest(t, encoded)
	manifest, err := ParseManifest(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return signed, verifier, manifest.Artifacts[0]
}

func resolvedArtifactForTest(artifact Artifact) ResolvedArtifact {
	return ResolvedArtifact{
		Path: artifact.Path, OS: artifact.OS, Arch: artifact.Arch,
		Variant: artifact.Variant, Role: artifact.Role,
		SHA256: artifact.SHA256, Size: artifact.Size,
	}
}

func sourceSelectionForTest(currentVersion string) Selection {
	return Selection{
		Repository: "cli-releases", Package: "edgecli", CurrentVersion: currentVersion,
		OS: "linux", Arch: "amd64", Role: "binary",
	}
}

func newSourceForTest(t *testing.T, baseURL string) *HTTPSource {
	t.Helper()
	source, err := NewHTTPSource(HTTPSourceConfig{
		BaseURL: baseURL, Product: "edgecli", InstallKey: sourceTestInstallKey,
		HTTPClient: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return fmt.Errorf("caller redirect policy should be replaced")
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func TestSourceResolutionSignedManifestIsDeepCopied(t *testing.T) {
	original := SignedManifest{
		KeyID: "key", Manifest: []byte("manifest"), Signature: []byte("signature"),
	}
	resolution := SourceResolution{signed: original}
	exposed := resolution.SignedManifest()
	exposed.Manifest[0] = 'X'
	exposed.Signature[0] = 'X'
	if bytes.Equal(exposed.Manifest, resolution.signed.Manifest) ||
		bytes.Equal(exposed.Signature, resolution.signed.Signature) {
		t.Fatal("SignedManifest() exposed mutable source bytes")
	}
}
