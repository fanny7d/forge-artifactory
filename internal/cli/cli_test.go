package cli

import (
	"bytes"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	api "superfan.myasustor.com/fanchao/artifact-repository/internal/api"
)

const testToken = "ar1.test-token"

func TestUploadComputesChecksumAndSendsProperties(t *testing.T) {
	content := []byte("artifactctl upload content")
	checksum := sha256Hex(content)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v1/repositories/source/artifacts/linux/arm64/tool" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		requireAuthorization(t, r)
		if r.ContentLength != int64(len(content)) || r.Header.Get("X-Checksum-Sha256") != checksum {
			t.Fatalf("upload headers = length %d checksum %q", r.ContentLength, r.Header.Get("X-Checksum-Sha256"))
		}
		properties, err := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Artifact-Properties"))
		if err != nil || string(properties) != `{"build":42}` {
			t.Fatalf("properties = %q, error %v", properties, err)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(body, content) {
			t.Fatalf("body = %q, error %v", body, err)
		}
		writeTestJSON(t, w, http.StatusCreated, testArtifact("source", "linux/arm64/tool", content))
	}))
	defer server.Close()

	localPath := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(localPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout := new(bytes.Buffer)
	err := Run(t.Context(), []string{
		"--url", server.URL,
		"upload", "--properties", `{"build":42}`, localPath, "source/linux/arm64/tool",
	}, stdout, io.Discard, testLookup)
	if err != nil {
		t.Fatal(err)
	}
	var result api.Artifact
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if result.Sha256 != checksum || result.Size != int64(len(content)) {
		t.Fatalf("output = %+v", result)
	}
}

func TestDownloadVerifiesAndAtomicallyInstallsFile(t *testing.T) {
	content := []byte("verified artifact bytes")
	server := artifactDownloadServer(t, content, nil)
	defer server.Close()

	destination := filepath.Join(t.TempDir(), "nested", "tool")
	stderr := new(bytes.Buffer)
	err := Run(t.Context(), []string{
		"--url", server.URL,
		"download", "-o", destination, "source/linux/arm64/tool",
	}, io.Discard, stderr, testLookup)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(destination)
	if err != nil || !bytes.Equal(actual, content) {
		t.Fatalf("download = %q, error %v", actual, err)
	}
	if !strings.Contains(stderr.String(), sha256Hex(content)) {
		t.Fatalf("status output = %q", stderr.String())
	}

	err = Run(t.Context(), []string{
		"--url", server.URL,
		"download", "-o", destination, "source/linux/arm64/tool",
	}, io.Discard, io.Discard, testLookup)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second download error = %v", err)
	}
}

func TestDownloadChecksumMismatchLeavesNoOutputFile(t *testing.T) {
	content := []byte("corrupt bytes")
	metadataContent := []byte("expected bytes")
	server := artifactDownloadServer(t, content, metadataContent)
	defer server.Close()
	destination := filepath.Join(t.TempDir(), "tool")

	err := Run(t.Context(), []string{
		"--url", server.URL,
		"download", "-o", destination, "source/linux/arm64/tool",
	}, io.Discard, io.Discard, testLookup)
	if err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("download error = %v", err)
	}
	if _, statErr := os.Stat(destination); !os.IsNotExist(statErr) {
		t.Fatalf("output exists after failed verification: %v", statErr)
	}
}

func TestRedirectDownloadDoesNotForwardBearerToken(t *testing.T) {
	content := []byte("redirected artifact")
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authorization := r.Header.Get("Authorization"); authorization != "" {
			t.Fatalf("Authorization leaked to storage: %q", authorization)
		}
		_, _ = w.Write(content)
	}))
	defer storage.Close()
	apiServer := artifactDownloadServer(t, content, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", storage.URL+"/presigned")
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	defer apiServer.Close()
	destination := filepath.Join(t.TempDir(), "tool")

	if err := Run(t.Context(), []string{
		"--url", apiServer.URL,
		"download", "--redirect", "-o", destination, "source/linux/arm64/tool",
	}, io.Discard, io.Discard, testLookup); err != nil {
		t.Fatal(err)
	}
}

func TestPullVerifiesSignedManifestBeforeDownload(t *testing.T) {
	content := []byte("signed stable artifact")
	publicKey, privateKey, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	publicKeyPath := filepath.Join(t.TempDir(), "public.pem")
	if err := os.WriteFile(publicKeyPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := signedManifest{
		SchemaVersion: 1,
		Repository:    "source",
		Package:       "tool",
		Version:       "1.0.0",
		PublishedAt:   time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC),
		Artifacts: []signedManifestArtifact{{
			Path: "linux/arm64/tool", Filename: "tool", OS: "linux", Arch: "arm64", Role: "binary", MediaType: "application/octet-stream",
			SHA256: sha256Hex(content), Size: int64(len(content)),
		}},
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := sha256.Sum256(publicKey)
	resolved := api.ResolveResponse{
		Version:   "1.0.0",
		KeyId:     "ed25519:" + hex.EncodeToString(fingerprint[:]),
		Manifest:  base64.RawURLEncoding.EncodeToString(manifestBytes),
		Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, manifestBytes)),
		Artifact: api.ResolveArtifact{
			Path: "linux/arm64/tool", Os: "linux", Arch: "arm64", Role: "binary",
			Sha256: sha256Hex(content), Size: int64(len(content)),
		},
		DownloadUrl: "/download/signed-tool",
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireAuthorization(t, r)
		switch r.URL.Path {
		case "/api/v1/repositories/source/packages/tool/channels/stable/resolve":
			if r.URL.Query().Get("os") != "linux" || r.URL.Query().Get("arch") != "arm64" || r.URL.Query().Get("role") != "binary" {
				t.Fatalf("resolve query = %s", r.URL.RawQuery)
			}
			writeTestJSON(t, w, http.StatusOK, resolved)
		case "/download/signed-tool":
			_, _ = w.Write(content)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	destination := filepath.Join(t.TempDir(), "tool")

	if err := Run(t.Context(), []string{
		"--url", server.URL,
		"pull", "--public-key", publicKeyPath, "--os", "linux", "--arch", "arm64", "-o", destination, "source/tool",
	}, io.Discard, io.Discard, testLookup); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(destination)
	if err != nil || !bytes.Equal(actual, content) {
		t.Fatalf("pulled content = %q, error %v", actual, err)
	}
}

func TestInspectReturnsStructuredAPIProblem(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		detail := "artifact is intentionally hidden"
		writeTestJSON(t, w, http.StatusNotFound, api.Problem{
			Type: "about:blank", Title: "Not Found", Status: http.StatusNotFound,
			Code: "not-found", Detail: &detail, RequestId: "request-123",
		})
	}))
	defer server.Close()
	err := Run(t.Context(), []string{
		"--url", server.URL, "inspect", "source/linux/arm64/tool",
	}, io.Discard, io.Discard, testLookup)
	if err == nil || !strings.Contains(err.Error(), "not-found") || !strings.Contains(err.Error(), "request-123") {
		t.Fatalf("inspect error = %v", err)
	}
}

func TestResolvedNetworkPathCannotReceiveBearerToken(t *testing.T) {
	client, err := newClient("https://repository.example.test", testToken)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.downloadResolved(t.Context(), "//attacker.example.test/artifact")
	if err == nil || !strings.Contains(err.Error(), "absolute API path") {
		t.Fatalf("downloadResolved error = %v", err)
	}
}

func TestPropertiesRejectTrailingJSON(t *testing.T) {
	if _, err := encodeProperties(`{"ok":true} []`); err == nil {
		t.Fatal("encodeProperties accepted trailing JSON")
	}
}

func artifactDownloadServer(t *testing.T, content []byte, override any) *httptest.Server {
	t.Helper()
	metadataContent := content
	var downloadHandler func(http.ResponseWriter, *http.Request)
	switch value := override.(type) {
	case nil:
	case []byte:
		metadataContent = value
	case func(http.ResponseWriter, *http.Request):
		downloadHandler = value
	default:
		t.Fatalf("unsupported server override %T", override)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireAuthorization(t, r)
		switch r.URL.Path {
		case "/api/v1/repositories/source/metadata/linux/arm64/tool":
			writeTestJSON(t, w, http.StatusOK, testArtifact("source", "linux/arm64/tool", metadataContent))
		case "/api/v1/repositories/source/artifacts/linux/arm64/tool":
			if downloadHandler != nil {
				downloadHandler(w, r)
				return
			}
			_, _ = w.Write(content)
		default:
			http.NotFound(w, r)
		}
	}))
}

func testArtifact(repository, path string, content []byte) api.Artifact {
	parts := strings.Split(path, "/")
	return api.Artifact{
		Id: uuid.MustParse("11111111-1111-4111-8111-111111111111"), Repository: repository,
		Path: path, Filename: parts[len(parts)-1], MediaType: "application/octet-stream",
		Size: int64(len(content)), Sha256: sha256Hex(content), Properties: map[string]any{},
		CreatedBy: uuid.MustParse("22222222-2222-4222-8222-222222222222"),
		CreatedAt: time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC),
	}
}

func requireAuthorization(t *testing.T, r *http.Request) {
	t.Helper()
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		t.Fatalf("Authorization = %q", r.Header.Get("Authorization"))
	}
}

func writeTestJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func sha256Hex(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

func testLookup(name string) (string, bool) {
	if name == "ARTIFACT_REPOSITORY_TOKEN" {
		return testToken, true
	}
	return "", false
}
