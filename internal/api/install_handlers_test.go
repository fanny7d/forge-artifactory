package api

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	artifactdomain "superfan.myasustor.com/fanchao/artifact-repository/internal/artifact"
	identity "superfan.myasustor.com/fanchao/artifact-repository/internal/auth"
	channeldomain "superfan.myasustor.com/fanchao/artifact-repository/internal/channel"
	productdomain "superfan.myasustor.com/fanchao/artifact-repository/internal/product"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/ratelimit"
	releasedomain "superfan.myasustor.com/fanchao/artifact-repository/internal/release"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/storage"
)

func TestInstallResolveBindsInstallKeyToSlugAndEscapesDownloadURL(t *testing.T) {
	key := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	product := installTestProduct(key)
	manifest := []byte(`{"schemaVersion":2}`)
	signature := []byte{0xfb, 0x01, 0xff}
	channels := &installChannelStub{result: channeldomain.Resolution{
		Version: "1.2.3", Manifest: manifest, KeyID: "release-key",
		Signature: signature,
		Artifact: channeldomain.ResolvedArtifact{
			Path: "linux/amd64/musl+v1/edgectl", OS: "linux", Arch: "amd64",
			Variant: "musl+v1", Role: "binary",
			SHA256: strings.Repeat("a", 64), Size: 9,
		},
	}}
	products := &productServiceStub{installKeyResult: product}
	handler := NewServer(Dependencies{
		Readiness: &readinessProbe{}, Products: products, Channels: channels,
		Artifacts: &installArtifactStub{},
	})

	request := httptest.NewRequest(
		http.MethodGet,
		"/i/"+key.String()+"/edgecli/resolve?os=linux&arch=amd64&variant=musl%2Bv1",
		nil,
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var resolved ResolveResponse
	if err := json.Unmarshal(response.Body.Bytes(), &resolved); err != nil {
		t.Fatalf("decode resolve response: %v", err)
	}
	if resolved.Manifest != base64.RawURLEncoding.EncodeToString(manifest) ||
		resolved.Signature != base64.RawURLEncoding.EncodeToString(signature) {
		t.Fatalf("signed fields = manifest %q signature %q", resolved.Manifest, resolved.Signature)
	}
	downloadURL, err := url.Parse(resolved.DownloadUrl)
	if err != nil {
		t.Fatalf("parse download URL %q: %v", resolved.DownloadUrl, err)
	}
	if downloadURL.Path != "/i/"+key.String()+"/edgecli/download" ||
		downloadURL.Query().Get("os") != "linux" ||
		downloadURL.Query().Get("arch") != "amd64" ||
		downloadURL.Query().Get("variant") != "musl+v1" ||
		downloadURL.Query().Get("version") != "1.2.3" ||
		downloadURL.Query().Get("sha256") != strings.Repeat("a", 64) {
		t.Fatalf("download URL = %q, decoded query = %#v", resolved.DownloadUrl, downloadURL.Query())
	}
	if products.installKey != key {
		t.Fatalf("install key lookup = %s, want %s", products.installKey, key)
	}
	if channels.request.RepositoryKey != "cli-releases" ||
		channels.request.PackageName != "edgecli" ||
		channels.request.ChannelName != "stable" ||
		channels.request.OS != "linux" ||
		channels.request.Arch != "amd64" ||
		channels.request.Variant != "musl+v1" ||
		channels.request.Role != "binary" ||
		channels.request.Redirect == nil || *channels.request.Redirect ||
		!channels.request.Actor.Scopes.Has(identity.ScopeAdmin) {
		t.Fatalf("channel resolve request = %+v", channels.request)
	}

	wrongSlugRequest := httptest.NewRequest(
		http.MethodGet,
		"/i/"+key.String()+"/another-cli/resolve?os=linux&arch=amd64",
		nil,
	)
	wrongSlugResponse := httptest.NewRecorder()
	handler.ServeHTTP(wrongSlugResponse, wrongSlugRequest)
	if wrongSlugResponse.Code != http.StatusNotFound {
		t.Fatalf("wrong slug status = %d, body = %s", wrongSlugResponse.Code, wrongSlugResponse.Body.String())
	}
	if channels.calls != 1 {
		t.Fatalf("channel Resolve() calls = %d, want 1", channels.calls)
	}

	invalidKeyRequest := httptest.NewRequest(
		http.MethodGet,
		"/i/not-a-uuid/edgecli/resolve?os=linux&arch=amd64",
		nil,
	)
	invalidKeyResponse := httptest.NewRecorder()
	handler.ServeHTTP(invalidKeyResponse, invalidKeyRequest)
	if invalidKeyResponse.Code != http.StatusNotFound {
		t.Fatalf("invalid key status = %d, body = %s", invalidKeyResponse.Code, invalidKeyResponse.Body.String())
	}
	if products.installKeyCalls != 2 {
		t.Fatalf("install key lookups = %d, want 2 (valid key and wrong slug only)", products.installKeyCalls)
	}
}

func TestInstallDownloadReturnsArtifactAndInstallMetadata(t *testing.T) {
	key := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	product := installTestProduct(key)
	payload := []byte("#!/bin/sh\necho edge\n")
	createdAt := time.Date(2026, 7, 23, 11, 0, 0, 0, time.UTC)
	checksum := strings.Repeat("b", 64)
	manifest := installManifest("linux", "amd64", "", "self-replace", "raw")
	channels := &installChannelStub{result: channeldomain.Resolution{
		Version: "2.0.0", Manifest: manifest,
		Artifact: channeldomain.ResolvedArtifact{
			Path: "linux/amd64/edgectl", OS: "linux", Arch: "amd64", Role: "binary",
			SHA256: checksum, Size: int64(len(payload)),
		},
	}}
	artifacts := &installArtifactStub{result: artifactdomain.OpenResult{
		Metadata: artifactdomain.Metadata{
			RepositoryKey: "cli-releases", Path: "linux/amd64/edgectl",
			Filename: "edgectl", MediaType: "application/octet-stream",
			Size: int64(len(payload)), SHA256: checksum, CreatedAt: createdAt,
		},
		Object: storage.Object{
			Body:   &seekReadCloser{Reader: bytes.NewReader(payload)},
			Seeker: bytes.NewReader(payload),
		},
	}}
	handler := NewServer(Dependencies{
		Readiness: &readinessProbe{},
		Products:  &productServiceStub{installKeyResult: product},
		Channels:  channels,
		Artifacts: artifacts,
	})

	request := httptest.NewRequest(
		http.MethodGet,
		"/i/"+key.String()+"/edgecli/download?os=linux&arch=amd64",
		nil,
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !bytes.Equal(response.Body.Bytes(), payload) {
		t.Fatalf("body = %q, want %q", response.Body.Bytes(), payload)
	}
	wantHeaders := map[string]string{
		"X-Checksum-Sha256":        checksum,
		"X-Forge-Version":          "2.0.0",
		"X-Forge-Install-Strategy": "self-replace",
		"X-Forge-Install-Format":   "raw",
		"Cache-Control":            "private, no-store",
		"Content-Disposition":      `attachment; filename="edgectl"`,
	}
	for name, want := range wantHeaders {
		if got := response.Header().Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	if artifacts.calls != 1 ||
		artifacts.request.RepositoryKey != "cli-releases" ||
		artifacts.request.RawPath != "linux/amd64/edgectl" ||
		artifacts.request.Redirect == nil || *artifacts.request.Redirect ||
		!artifacts.request.Actor.Scopes.Has(identity.ScopeAdmin) {
		t.Fatalf("artifact Open request = %+v, calls = %d", artifacts.request, artifacts.calls)
	}
}

func TestInstallDownloadPinsPublishedVersionInsteadOfResolvingStableAgain(t *testing.T) {
	key := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	product := installTestProduct(key)
	checksum := strings.Repeat("c", 64)
	payload := []byte("version-1.2.3")
	releases := &installReleaseStub{result: releasedomain.Release{
		Version: "1.2.3",
		State:   "published",
		Artifacts: []releasedomain.ReleaseArtifact{{
			OS: "linux", Arch: "amd64", Role: "binary",
			Artifact: releasedomain.Artifact{
				Path:   "products/edgecli/1.2.3/linux/amd64/edgecli",
				SHA256: checksum,
			},
			Install: &releasedomain.InstallSpec{
				Strategy: releasedomain.InstallStrategySelfReplace,
				Format:   releasedomain.InstallFormatRaw,
				Mode:     "0755",
			},
		}},
	}}
	channels := &installChannelStub{err: errors.New("stable must not be resolved for a pinned download")}
	artifacts := &installArtifactStub{result: artifactdomain.OpenResult{
		Metadata: artifactdomain.Metadata{
			Filename: "edgecli", MediaType: "application/octet-stream",
			CreatedAt: time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC),
		},
		Object: storage.Object{
			Body:   &seekReadCloser{Reader: bytes.NewReader(payload)},
			Seeker: bytes.NewReader(payload),
		},
	}}
	handler := NewServer(Dependencies{
		Readiness: &readinessProbe{},
		Products:  &productServiceStub{installKeyResult: product},
		Channels:  channels,
		Drafts:    releases,
		Artifacts: artifacts,
	})

	request := httptest.NewRequest(
		http.MethodGet,
		"/i/"+key.String()+"/edgecli/download?os=linux&arch=amd64&version=1.2.3&sha256="+checksum,
		nil,
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if channels.calls != 0 {
		t.Fatalf("stable Resolve() calls = %d, want 0", channels.calls)
	}
	if releases.calls != 1 || releases.version != "1.2.3" {
		t.Fatalf("release Get() calls/version = %d/%q", releases.calls, releases.version)
	}
	if artifacts.request.RawPath != "products/edgecli/1.2.3/linux/amd64/edgecli" {
		t.Fatalf("artifact path = %q", artifacts.request.RawPath)
	}
	if response.Header().Get("X-Checksum-Sha256") != checksum ||
		response.Header().Get("X-Forge-Version") != "1.2.3" {
		t.Fatalf("pinned headers = %#v", response.Header())
	}
}

func TestInstallScriptReturnsCurrentPlatformDispatcher(t *testing.T) {
	key := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	product := installTestProduct(key)
	handler := NewServer(Dependencies{
		Readiness: &readinessProbe{},
		Products:  &productServiceStub{installKeyResult: product},
		Channels:  &installChannelStub{},
		Artifacts: &installArtifactStub{},
	})

	request := httptest.NewRequest(http.MethodGet, "/i/"+key.String()+"/edgecli/install", nil)
	request.Host = "forge.internal:8443"
	request.Header.Set("X-Forwarded-Proto", "https")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Content-Type") != "text/x-shellscript; charset=utf-8" ||
		response.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("script headers = %#v", response.Header())
	}
	script := response.Body.String()
	required := []string{
		"#!/bin/sh\nset -eu",
		`case "$(uname -s)" in`,
		`case "$(uname -m)" in`,
		`script_file=$(mktemp "${TMPDIR:-/tmp}/forge-install-script.XXXXXX")`,
		`trap cleanup EXIT HUP INT TERM`,
		`https://forge.internal:8443/i/` + key.String() + `/edgecli/install`,
		`"${install_url}?os=${forge_os}&arch=${forge_arch}"`,
		`curl -fsSL --retry 2`,
		`/bin/sh "$script_file"`,
	}
	for _, fragment := range required {
		if !strings.Contains(script, fragment) {
			t.Fatalf("install script missing %q:\n%s", fragment, script)
		}
	}
	for _, forbidden := range []string{"sudo ", "Authorization:", "Bearer "} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("install script contains forbidden fragment %q:\n%s", forbidden, script)
		}
	}
}

func TestInstallScriptsPreserveGraceInstallKeyWithoutExposingCurrentKey(t *testing.T) {
	requestKey := uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	currentKey := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	product := installTestProduct(currentKey)
	checksum := strings.Repeat("a", 64)
	channels := &installChannelStub{result: channeldomain.Resolution{
		Version:  "1.2.3",
		Manifest: installManifest("linux", "amd64", "", "self-replace", "raw"),
		Artifact: channeldomain.ResolvedArtifact{
			Path: "linux/amd64/edgectl", OS: "linux", Arch: "amd64", Role: "binary",
			SHA256: checksum, Size: 9,
		},
	}}
	handler := NewServer(Dependencies{
		Readiness: &readinessProbe{},
		Products:  &productServiceStub{installKeyResult: product},
		Channels:  channels,
		Artifacts: &installArtifactStub{},
	})

	requests := []string{
		"/i/" + requestKey.String() + "/edgecli/install",
		"/i/" + requestKey.String() + "/edgecli/install?os=linux&arch=amd64",
	}
	for _, target := range requests {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		request.Host = "forge.internal"
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d, body = %s", target, response.Code, response.Body.String())
		}
		script := response.Body.String()
		if !strings.Contains(script, requestKey.String()) {
			t.Fatalf("%s script does not preserve request/grace key:\n%s", target, script)
		}
		if strings.Contains(script, currentKey.String()) {
			t.Fatalf("%s script exposed current install key:\n%s", target, script)
		}
	}
}

func TestInstallRawScriptExecutesPinnedChecksumCheckedAtomicInstall(t *testing.T) {
	key := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	payload := []byte("#!/bin/sh\nprintf 'new-version\\n'\n")
	spec := releasedomain.InstallSpec{
		Strategy: releasedomain.InstallStrategySelfReplace,
		Format:   releasedomain.InstallFormatRaw,
		Mode:     "0755",
	}
	server := newExecutableInstallServer(t, key, "1.2.3", payload, spec, "")
	defer server.Close()

	script := fetchConcreteInstallScript(t, server.URL, key)
	required := []string{
		`version='1.2.3'`,
		`expected='` + sha256Hex(payload) + `'`,
		`version=1.2.3`,
		`sha256=` + sha256Hex(payload),
		`candidate=$(mktemp "$install_dir/.${command_name}.new.XXXXXX")`,
		`cp -p "$target" "$target.old"`,
		`mv -f "$candidate" "$target"`,
	}
	for _, fragment := range required {
		if !strings.Contains(script, fragment) {
			t.Fatalf("concrete raw script missing %q:\n%s", fragment, script)
		}
	}

	installDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatalf("create install directory: %v", err)
	}
	target := filepath.Join(installDir, "edgectl")
	if err := os.WriteFile(target, []byte("old-version"), 0o700); err != nil {
		t.Fatalf("write old executable: %v", err)
	}
	output, err := runInstallScript(t, script, map[string]string{"FORGE_INSTALL_DIR": installDir})
	if err != nil {
		t.Fatalf("run raw install script: %v\n%s", err, output)
	}
	installed, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read installed executable: %v", err)
	}
	if !bytes.Equal(installed, payload) {
		t.Fatalf("installed payload = %q, want %q", installed, payload)
	}
	backup, err := os.ReadFile(target + ".old")
	if err != nil {
		t.Fatalf("read backup executable: %v", err)
	}
	if string(backup) != "old-version" {
		t.Fatalf("backup payload = %q", backup)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat installed executable: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("installed mode = %04o, want 0755", info.Mode().Perm())
	}
}

func TestInstallRawScriptRejectsChecksumMismatchBeforeReplacement(t *testing.T) {
	key := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	payload := []byte("#!/bin/sh\necho tampered\n")
	spec := releasedomain.InstallSpec{
		Strategy: releasedomain.InstallStrategySelfReplace,
		Format:   releasedomain.InstallFormatRaw,
		Mode:     "0755",
	}
	server := newExecutableInstallServer(t, key, "1.2.3", payload, spec, strings.Repeat("0", 64))
	defer server.Close()

	script := fetchConcreteInstallScript(t, server.URL, key)
	installDir := filepath.Join(t.TempDir(), "bin")
	output, err := runInstallScript(t, script, map[string]string{"FORGE_INSTALL_DIR": installDir})
	if err == nil {
		t.Fatalf("checksum-mismatched install unexpectedly succeeded:\n%s", output)
	}
	if !strings.Contains(output, "checksum verification failed") {
		t.Fatalf("checksum failure output = %q", output)
	}
	if _, statErr := os.Lstat(filepath.Join(installDir, "edgectl")); !os.IsNotExist(statErr) {
		t.Fatalf("target exists after checksum failure: %v", statErr)
	}
}

func TestInstallBundleScriptExtractsActivatesAndRunsSignedHooks(t *testing.T) {
	key := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	hook := `#!/bin/sh
printf '%s:%s:%s\n' "$FORGEUPDATE_HOOK_PHASE" "$FORGEUPDATE_VERSION" "$1" >> "$HOME/hook.log"
`
	payload := tarGZInstallFixture(t, []tarInstallEntry{
		{name: "bin/edgectl", mode: 0o755, body: "#!/bin/sh\nprintf 'bundle-ok\\n'\n"},
		{name: "hooks/install", mode: 0o755, body: hook},
	})
	spec := releasedomain.InstallSpec{
		Strategy:   releasedomain.InstallStrategyBundle,
		Format:     releasedomain.InstallFormatTarGZ,
		Entrypoint: "bin/edgectl",
		Mode:       "0755",
		Hooks: []releasedomain.InstallHook{
			{Phase: releasedomain.HookPhasePreflight, Path: "hooks/install", Args: []string{"pre"}, TimeoutSeconds: 5},
			{Phase: releasedomain.HookPhasePostInstall, Path: "hooks/install", Args: []string{"post"}, TimeoutSeconds: 5},
			{Phase: releasedomain.HookPhaseVerify, Path: "hooks/install", Args: []string{"verify"}, TimeoutSeconds: 5},
		},
	}
	server := newExecutableInstallServer(t, key, "2.0.0", payload, spec, "")
	defer server.Close()

	script := fetchConcreteInstallScript(t, server.URL, key)
	for _, fragment := range []string{
		`validate_archive_names`,
		`archive contains a link or special file`,
		`atomic_replace_symlink`,
		`run_hook 'preflight' 5 "$staging" "$staging"/'hooks/install' 'pre'`,
		`run_hook 'post-install' 5 "$target" "$target"/'hooks/install' 'post'`,
		`run_hook 'verify' 5 "$target" "$target"/'hooks/install' 'verify'`,
	} {
		if !strings.Contains(script, fragment) {
			t.Fatalf("bundle script missing %q:\n%s", fragment, script)
		}
	}

	home := t.TempDir()
	root := filepath.Join(home, "share", "edgecli")
	installDir := filepath.Join(home, "bin")
	output, err := runInstallScript(t, script, map[string]string{
		"HOME":               home,
		"FORGE_INSTALL_ROOT": root,
		"FORGE_INSTALL_DIR":  installDir,
	})
	if err != nil {
		t.Fatalf("run bundle install script: %v\n%s", err, output)
	}
	versionPath := filepath.Join(root, "versions", "2.0.0")
	entrypoint := filepath.Join(versionPath, "bin", "edgectl")
	if content, readErr := os.ReadFile(entrypoint); readErr != nil ||
		!strings.Contains(string(content), "bundle-ok") {
		t.Fatalf("installed entrypoint content/error = %q/%v", content, readErr)
	}
	if target, readErr := os.Readlink(filepath.Join(root, "current")); readErr != nil || target != "versions/2.0.0" {
		t.Fatalf("current symlink target/error = %q/%v", target, readErr)
	}
	launcher := filepath.Join(installDir, "edgectl")
	if target, readErr := os.Readlink(launcher); readErr != nil ||
		target != filepath.Join(root, "current", "bin", "edgectl") {
		t.Fatalf("launcher symlink target/error = %q/%v", target, readErr)
	}
	command := exec.Command(launcher)
	commandOutput, commandErr := command.CombinedOutput()
	if commandErr != nil || string(commandOutput) != "bundle-ok\n" {
		t.Fatalf("run stable launcher: output=%q err=%v", commandOutput, commandErr)
	}
	hookLog, err := os.ReadFile(filepath.Join(home, "hook.log"))
	if err != nil {
		t.Fatalf("read hook log: %v", err)
	}
	if got, want := string(hookLog), "preflight:2.0.0:pre\npost-install:2.0.0:post\nverify:2.0.0:verify\n"; got != want {
		t.Fatalf("hook log = %q, want %q", got, want)
	}
}

func TestInstallBundleZIPScriptExtractsAndActivates(t *testing.T) {
	if _, err := exec.LookPath("unzip"); err != nil {
		t.Skip("unzip is required for the executable ZIP bootstrap test")
	}
	key := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	payload := zipInstallFixture(t, []tarInstallEntry{{
		name: "bin/edgectl", mode: 0o755, body: "#!/bin/sh\nprintf 'zip-ok\\n'\n",
	}})
	spec := releasedomain.InstallSpec{
		Strategy: releasedomain.InstallStrategyBundle,
		Format:   releasedomain.InstallFormatZIP, Entrypoint: "bin/edgectl", Mode: "0755",
	}
	server := newExecutableInstallServer(t, key, "2.1.0", payload, spec, "")
	defer server.Close()
	script := fetchConcreteInstallScript(t, server.URL, key)
	home := t.TempDir()
	root := filepath.Join(home, "share", "edgecli")
	installDir := filepath.Join(home, "bin")
	output, err := runInstallScript(t, script, map[string]string{
		"HOME": home, "FORGE_INSTALL_ROOT": root, "FORGE_INSTALL_DIR": installDir,
	})
	if err != nil {
		t.Fatalf("run ZIP bundle install script: %v\n%s", err, output)
	}
	launcher := filepath.Join(installDir, "edgectl")
	commandOutput, commandErr := exec.Command(launcher).CombinedOutput()
	if commandErr != nil || string(commandOutput) != "zip-ok\n" {
		t.Fatalf("run ZIP launcher: output=%q err=%v", commandOutput, commandErr)
	}
	if currentTarget, readErr := os.Readlink(filepath.Join(root, "current")); readErr != nil ||
		currentTarget != "versions/2.1.0" {
		t.Fatalf("ZIP current symlink target/error = %q/%v", currentTarget, readErr)
	}
}

func TestInstallBundleScriptRejectsTraversalAndRollsBackFailedHook(t *testing.T) {
	key := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	t.Run("archive traversal", func(t *testing.T) {
		payload := tarGZInstallFixture(t, []tarInstallEntry{
			{name: "../../escaped", mode: 0o644, body: "escaped"},
			{name: "bin/edgectl", mode: 0o755, body: "#!/bin/sh\nexit 0\n"},
		})
		spec := releasedomain.InstallSpec{
			Strategy: releasedomain.InstallStrategyBundle, Format: releasedomain.InstallFormatTarGZ,
			Entrypoint: "bin/edgectl", Mode: "0755",
		}
		server := newExecutableInstallServer(t, key, "2.0.0", payload, spec, "")
		defer server.Close()
		script := fetchConcreteInstallScript(t, server.URL, key)
		home := t.TempDir()
		root := filepath.Join(home, "share", "edgecli")
		output, err := runInstallScript(t, script, map[string]string{
			"HOME": home, "FORGE_INSTALL_ROOT": root, "FORGE_INSTALL_DIR": filepath.Join(home, "bin"),
		})
		if err == nil {
			t.Fatalf("traversal archive unexpectedly installed:\n%s", output)
		}
		if _, statErr := os.Lstat(filepath.Join(root, "escaped")); !os.IsNotExist(statErr) {
			t.Fatalf("archive escaped staging root: %v", statErr)
		}
		if _, statErr := os.Lstat(filepath.Join(root, "versions", "2.0.0")); !os.IsNotExist(statErr) {
			t.Fatalf("version committed after unsafe archive: %v", statErr)
		}
	})

	t.Run("preflight cannot replace entrypoint with symlink", func(t *testing.T) {
		hook := `#!/bin/sh
rm -f bin/edgectl
ln -s /bin/sh bin/edgectl
`
		payload := tarGZInstallFixture(t, []tarInstallEntry{
			{name: "bin/edgectl", mode: 0o755, body: "#!/bin/sh\nexit 0\n"},
			{name: "hooks/preflight", mode: 0o755, body: hook},
		})
		spec := releasedomain.InstallSpec{
			Strategy: releasedomain.InstallStrategyBundle, Format: releasedomain.InstallFormatTarGZ,
			Entrypoint: "bin/edgectl", Mode: "0755",
			Hooks: []releasedomain.InstallHook{{
				Phase: releasedomain.HookPhasePreflight, Path: "hooks/preflight", TimeoutSeconds: 5,
			}},
		}
		server := newExecutableInstallServer(t, key, "2.0.0", payload, spec, "")
		defer server.Close()
		script := fetchConcreteInstallScript(t, server.URL, key)
		home := t.TempDir()
		root := filepath.Join(home, "share", "edgecli")
		output, err := runInstallScript(t, script, map[string]string{
			"HOME": home, "FORGE_INSTALL_ROOT": root, "FORGE_INSTALL_DIR": filepath.Join(home, "bin"),
		})
		if err == nil {
			t.Fatalf("unsafe preflight mutation unexpectedly installed:\n%s", output)
		}
		if !strings.Contains(output, "extracted bundle contains a link or special file") {
			t.Fatalf("preflight mutation failure output = %q", output)
		}
		if _, statErr := os.Lstat(filepath.Join(root, "versions", "2.0.0")); !os.IsNotExist(statErr) {
			t.Fatalf("version committed after unsafe preflight mutation: %v", statErr)
		}
	})

	t.Run("verify hook rollback", func(t *testing.T) {
		hook := `#!/bin/sh
if [ "$FORGEUPDATE_HOOK_PHASE" = verify ]; then
  exit 19
fi
`
		payload := tarGZInstallFixture(t, []tarInstallEntry{
			{name: "bin/edgectl", mode: 0o755, body: "#!/bin/sh\nprintf 'new\\n'\n"},
			{name: "hooks/install", mode: 0o755, body: hook},
		})
		spec := releasedomain.InstallSpec{
			Strategy: releasedomain.InstallStrategyBundle, Format: releasedomain.InstallFormatTarGZ,
			Entrypoint: "bin/edgectl", Mode: "0755",
			Hooks: []releasedomain.InstallHook{
				{Phase: releasedomain.HookPhasePostInstall, Path: "hooks/install", TimeoutSeconds: 5},
				{Phase: releasedomain.HookPhaseVerify, Path: "hooks/install", TimeoutSeconds: 5},
			},
		}
		server := newExecutableInstallServer(t, key, "2.0.0", payload, spec, "")
		defer server.Close()
		script := fetchConcreteInstallScript(t, server.URL, key)
		home := t.TempDir()
		root := filepath.Join(home, "share", "edgecli")
		installDir := filepath.Join(home, "bin")
		oldVersion := filepath.Join(root, "versions", "1.0.0")
		if err := os.MkdirAll(filepath.Join(oldVersion, "bin"), 0o755); err != nil {
			t.Fatalf("create old version: %v", err)
		}
		if err := os.WriteFile(filepath.Join(oldVersion, "bin", "edgectl"), []byte("old"), 0o755); err != nil {
			t.Fatalf("write old version: %v", err)
		}
		if err := os.MkdirAll(installDir, 0o755); err != nil {
			t.Fatalf("create launcher directory: %v", err)
		}
		if err := os.Symlink("versions/1.0.0", filepath.Join(root, "current")); err != nil {
			t.Fatalf("create old current symlink: %v", err)
		}
		oldLauncherTarget := filepath.Join(root, "current", "bin", "edgectl")
		if err := os.Symlink(oldLauncherTarget, filepath.Join(installDir, "edgectl")); err != nil {
			t.Fatalf("create old launcher: %v", err)
		}

		output, err := runInstallScript(t, script, map[string]string{
			"HOME": home, "FORGE_INSTALL_ROOT": root, "FORGE_INSTALL_DIR": installDir,
		})
		if err == nil {
			t.Fatalf("failing verify hook unexpectedly succeeded:\n%s", output)
		}
		if currentTarget, readErr := os.Readlink(filepath.Join(root, "current")); readErr != nil ||
			currentTarget != "versions/1.0.0" {
			t.Fatalf("current was not rolled back: target=%q err=%v\n%s", currentTarget, readErr, output)
		}
		if launcherTarget, readErr := os.Readlink(filepath.Join(installDir, "edgectl")); readErr != nil ||
			launcherTarget != oldLauncherTarget {
			t.Fatalf("launcher was not rolled back: target=%q err=%v\n%s", launcherTarget, readErr, output)
		}
		if _, statErr := os.Lstat(filepath.Join(root, "versions", "2.0.0")); !os.IsNotExist(statErr) {
			t.Fatalf("failed version remains after rollback: %v\n%s", statErr, output)
		}
	})
}

func TestInstallScriptRejectsInvalidHostAndForwardedScheme(t *testing.T) {
	key := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	dependencies := Dependencies{
		Readiness: &readinessProbe{},
		Products:  &productServiceStub{installKeyResult: installTestProduct(key)},
		Channels:  &installChannelStub{},
		Artifacts: &installArtifactStub{},
	}

	tests := []struct {
		name      string
		host      string
		forwarded string
	}{
		{name: "invalid host", host: "forge_internal"},
		{name: "invalid forwarded scheme", host: "forge.internal", forwarded: "javascript"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := NewServer(dependencies)
			request := httptest.NewRequest(http.MethodGet, "/i/"+key.String()+"/edgecli/install", nil)
			request.Host = tt.host
			if tt.forwarded != "" {
				request.Header.Set("X-Forwarded-Proto", tt.forwarded)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), tt.host) ||
				(tt.forwarded != "" && strings.Contains(response.Body.String(), tt.forwarded)) {
				t.Fatalf("problem response reflected invalid input: %s", response.Body.String())
			}
		})
	}
}

func TestInstallEndpointDoesNotHideInstallKeyLookupFailureAsNotFound(t *testing.T) {
	key := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	products := &productServiceStub{installKeyError: errors.New("database unavailable")}
	channels := &installChannelStub{}
	handler := NewServer(Dependencies{
		Readiness: &readinessProbe{},
		Products:  products,
		Channels:  channels,
		Artifacts: &installArtifactStub{},
	})

	request := httptest.NewRequest(
		http.MethodGet,
		"/i/"+key.String()+"/edgecli/resolve?os=linux&arch=amd64",
		nil,
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", response.Code, response.Body.String())
	}
	if channels.calls != 0 {
		t.Fatalf("channel Resolve() calls = %d, want 0", channels.calls)
	}
}

func TestInstallEndpointsGlobalRateLimitRunsBeforeProductLookup(t *testing.T) {
	key := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	products := &productServiceStub{installKeyResult: installTestProduct(key)}
	limiter := &requestLimiterStub{decisions: []ratelimit.Decision{{RetryAfter: 1500 * time.Millisecond}}}
	handler := NewServer(Dependencies{
		Readiness:   &readinessProbe{},
		Products:    products,
		Channels:    &installChannelStub{},
		Artifacts:   &installArtifactStub{},
		RateLimiter: limiter,
	})

	request := httptest.NewRequest(http.MethodGet, "/i/"+key.String()+"/edgecli/install", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "2" {
		t.Fatalf("status/retry = %d/%q, body = %s", response.Code, response.Header().Get("Retry-After"), response.Body.String())
	}
	if len(limiter.tokens) != 1 || limiter.tokens[0] != installGlobalRateLimitKey ||
		len(limiter.classes) != 1 || limiter.classes[0] != ratelimit.ClassRead {
		t.Fatalf("limiter tokens/classes = %#v/%#v", limiter.tokens, limiter.classes)
	}
	if products.installKeyCalls != 0 {
		t.Fatalf("product lookups = %d, want 0", products.installKeyCalls)
	}
}

func TestInstallEndpointsInvalidKeyUsesOnlyGlobalRateLimitBucket(t *testing.T) {
	releases := 0
	products := &productServiceStub{}
	limiter := &requestLimiterStub{decisions: []ratelimit.Decision{{
		Allowed: true,
		Release: func() {
			releases++
		},
	}}}
	handler := NewServer(Dependencies{
		Readiness:   &readinessProbe{},
		Products:    products,
		Channels:    &installChannelStub{},
		Artifacts:   &installArtifactStub{},
		RateLimiter: limiter,
	})

	request := httptest.NewRequest(http.MethodGet, "/i/not-a-uuid/edgecli/install", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", response.Code, response.Body.String())
	}
	if len(limiter.tokens) != 1 || limiter.tokens[0] != installGlobalRateLimitKey {
		t.Fatalf("limiter tokens = %#v, want only global bucket", limiter.tokens)
	}
	if releases != 1 {
		t.Fatalf("global permit releases = %d, want 1", releases)
	}
	if products.installKeyCalls != 0 {
		t.Fatalf("product lookups = %d, want 0", products.installKeyCalls)
	}
}

func TestInstallEndpointsRotatingKeysShareGlobalBucketAndValidKeyGetsPerKeyBucket(t *testing.T) {
	keyA := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	keyB := uuid.MustParse("22222222-3333-4444-8555-666666666666")
	products := &productServiceStub{installKeyResult: installTestProduct(keyA)}
	releases := 0
	release := func() {
		releases++
	}
	limiter := &requestLimiterStub{decisions: []ratelimit.Decision{
		{Allowed: true, Release: release},
		{Allowed: true, Release: release},
		{RetryAfter: time.Second},
	}}
	handler := NewServer(Dependencies{
		Readiness:   &readinessProbe{},
		Products:    products,
		Channels:    &installChannelStub{},
		Artifacts:   &installArtifactStub{},
		RateLimiter: limiter,
	})

	firstRequest := httptest.NewRequest(http.MethodGet, "/i/"+keyA.String()+"/edgecli/install", nil)
	firstResponse := httptest.NewRecorder()
	handler.ServeHTTP(firstResponse, firstRequest)
	if firstResponse.Code != http.StatusOK {
		t.Fatalf("first status = %d, body = %s", firstResponse.Code, firstResponse.Body.String())
	}
	if releases != 2 {
		t.Fatalf("first global/per-key permit releases = %d, want 2", releases)
	}

	secondRequest := httptest.NewRequest(http.MethodGet, "/i/"+keyB.String()+"/edgecli/install", nil)
	secondResponse := httptest.NewRecorder()
	handler.ServeHTTP(secondResponse, secondRequest)
	if secondResponse.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want 429; body = %s", secondResponse.Code, secondResponse.Body.String())
	}
	wantTokens := []uuid.UUID{installGlobalRateLimitKey, keyA, installGlobalRateLimitKey}
	if len(limiter.tokens) != len(wantTokens) {
		t.Fatalf("limiter tokens = %#v, want %#v", limiter.tokens, wantTokens)
	}
	for index := range wantTokens {
		if limiter.tokens[index] != wantTokens[index] {
			t.Fatalf("limiter tokens = %#v, want %#v", limiter.tokens, wantTokens)
		}
	}
	if products.installKeyCalls != 1 {
		t.Fatalf("product lookups = %d, want only the globally accepted request", products.installKeyCalls)
	}
}

func TestInstallEndpointDoesNotAcquireGlobalBucketTwiceWhenKeysMatch(t *testing.T) {
	products := &productServiceStub{installKeyResult: installTestProduct(installGlobalRateLimitKey)}
	limiter := &requestLimiterStub{}
	handler := NewServer(Dependencies{
		Readiness:   &readinessProbe{},
		Products:    products,
		Channels:    &installChannelStub{},
		Artifacts:   &installArtifactStub{},
		RateLimiter: limiter,
	})

	request := httptest.NewRequest(
		http.MethodGet,
		"/i/"+installGlobalRateLimitKey.String()+"/edgecli/install",
		nil,
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if len(limiter.tokens) != 1 || limiter.tokens[0] != installGlobalRateLimitKey {
		t.Fatalf("limiter tokens = %#v, want exactly one global acquisition", limiter.tokens)
	}
	if products.installKeyCalls != 1 {
		t.Fatalf("product lookups = %d, want 1", products.installKeyCalls)
	}
}

func installTestProduct(key uuid.UUID) productdomain.Product {
	return productdomain.Product{
		Slug: "edgecli", RepositoryKey: "cli-releases", PackageName: "edgecli",
		CommandName: "edgectl", InstallKey: key,
	}
}

func installManifest(osValue, arch, variant, strategy, format string) []byte {
	document := map[string]any{
		"schemaVersion": 2,
		"artifacts": []map[string]any{{
			"os": osValue, "arch": arch, "variant": variant, "role": "binary",
			"install": map[string]any{"strategy": strategy, "format": format, "mode": "0755"},
		}},
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		panic(err)
	}
	return encoded
}

func installManifestWithSpec(spec releasedomain.InstallSpec) []byte {
	document := map[string]any{
		"schemaVersion": 2,
		"artifacts": []map[string]any{{
			"os": "linux", "arch": "amd64", "variant": "", "role": "binary",
			"install": spec,
		}},
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		panic(err)
	}
	return encoded
}

func newExecutableInstallServer(
	t *testing.T,
	key uuid.UUID,
	version string,
	payload []byte,
	spec releasedomain.InstallSpec,
	declaredSHA256 string,
) *httptest.Server {
	t.Helper()
	if declaredSHA256 == "" {
		declaredSHA256 = sha256Hex(payload)
	}
	artifactPath := "products/edgecli/" + version + "/linux/amd64/edgecli"
	channels := &installChannelStub{result: channeldomain.Resolution{
		Version:  version,
		Manifest: installManifestWithSpec(spec),
		Artifact: channeldomain.ResolvedArtifact{
			Path: artifactPath, OS: "linux", Arch: "amd64", Role: "binary",
			SHA256: declaredSHA256, Size: int64(len(payload)),
		},
	}}
	releases := &installReleaseStub{result: releasedomain.Release{
		Version: version,
		State:   "published",
		Artifacts: []releasedomain.ReleaseArtifact{{
			OS: "linux", Arch: "amd64", Role: "binary",
			Artifact: releasedomain.Artifact{
				Path: artifactPath, Filename: "edgecli",
				MediaType: "application/octet-stream", Size: int64(len(payload)),
				SHA256: declaredSHA256,
			},
			Install: &spec,
		}},
	}}
	artifacts := &installArtifactStub{result: artifactdomain.OpenResult{
		Metadata: artifactdomain.Metadata{
			RepositoryKey: "cli-releases", Path: artifactPath, Filename: "edgecli",
			MediaType: "application/octet-stream", Size: int64(len(payload)),
			SHA256: declaredSHA256, CreatedAt: time.Now().UTC(),
		},
		Object: storage.Object{
			Body:   &seekReadCloser{Reader: bytes.NewReader(payload)},
			Seeker: bytes.NewReader(payload),
		},
	}}
	handler := NewServer(Dependencies{
		Readiness: &readinessProbe{},
		Products:  &productServiceStub{installKeyResult: installTestProduct(key)},
		Channels:  channels,
		Drafts:    releases,
		Artifacts: artifacts,
	})
	return httptest.NewServer(handler)
}

func fetchConcreteInstallScript(t *testing.T, baseURL string, key uuid.UUID) string {
	t.Helper()
	response, err := http.Get(baseURL + "/i/" + key.String() + "/edgecli/install?os=linux&arch=amd64")
	if err != nil {
		t.Fatalf("fetch concrete install script: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read concrete install script: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("concrete install status = %d, body = %s", response.StatusCode, body)
	}
	return string(body)
}

func runInstallScript(t *testing.T, script string, overrides map[string]string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/sh")
	command.Stdin = strings.NewReader(script)
	command.Env = installTestEnvironment(overrides)
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("install script timed out: %v\n%s", ctx.Err(), output)
	}
	return string(output), err
}

func installTestEnvironment(overrides map[string]string) []string {
	environment := os.Environ()
	filtered := make([]string, 0, len(environment)+len(overrides))
	for _, value := range environment {
		name, _, ok := strings.Cut(value, "=")
		if _, overridden := overrides[name]; ok && overridden {
			continue
		}
		filtered = append(filtered, value)
	}
	for name, value := range overrides {
		filtered = append(filtered, name+"="+value)
	}
	return filtered
}

func sha256Hex(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

type tarInstallEntry struct {
	name string
	mode int64
	body string
}

func tarGZInstallFixture(t *testing.T, entries []tarInstallEntry) []byte {
	t.Helper()
	var encoded bytes.Buffer
	compressor := gzip.NewWriter(&encoded)
	archive := tar.NewWriter(compressor)
	for _, entry := range entries {
		header := &tar.Header{
			Name: entry.name,
			Mode: entry.mode,
			Size: int64(len(entry.body)),
		}
		if err := archive.WriteHeader(header); err != nil {
			t.Fatalf("write tar header %q: %v", entry.name, err)
		}
		if _, err := archive.Write([]byte(entry.body)); err != nil {
			t.Fatalf("write tar body %q: %v", entry.name, err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatalf("close tar archive: %v", err)
	}
	if err := compressor.Close(); err != nil {
		t.Fatalf("close gzip stream: %v", err)
	}
	return encoded.Bytes()
}

func zipInstallFixture(t *testing.T, entries []tarInstallEntry) []byte {
	t.Helper()
	var encoded bytes.Buffer
	archive := zip.NewWriter(&encoded)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		header.SetMode(os.FileMode(entry.mode))
		writer, err := archive.CreateHeader(header)
		if err != nil {
			t.Fatalf("write ZIP header %q: %v", entry.name, err)
		}
		if _, err := writer.Write([]byte(entry.body)); err != nil {
			t.Fatalf("write ZIP body %q: %v", entry.name, err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatalf("close ZIP archive: %v", err)
	}
	return encoded.Bytes()
}

type installChannelStub struct {
	request channeldomain.ResolveRequest
	result  channeldomain.Resolution
	err     error
	calls   int
}

func (s *installChannelStub) Promote(context.Context, channeldomain.PromoteRequest) (channeldomain.Revision, error) {
	return channeldomain.Revision{}, nil
}

func (s *installChannelStub) Current(context.Context, identity.Actor, string, string, string) (channeldomain.Channel, error) {
	return channeldomain.Channel{}, nil
}

func (s *installChannelStub) History(context.Context, identity.Actor, string, string, string, channeldomain.HistoryRequest) (channeldomain.HistoryPage, error) {
	return channeldomain.HistoryPage{}, nil
}

func (s *installChannelStub) Resolve(_ context.Context, request channeldomain.ResolveRequest) (channeldomain.Resolution, error) {
	s.calls++
	s.request = request
	return s.result, s.err
}

type installArtifactStub struct {
	request artifactdomain.OpenRequest
	result  artifactdomain.OpenResult
	err     error
	calls   int
}

type installReleaseStub struct {
	result  releasedomain.Release
	err     error
	version string
	calls   int
}

func (s *installReleaseStub) Get(
	_ context.Context,
	_ identity.Actor,
	_, _, version string,
) (releasedomain.Release, error) {
	s.calls++
	s.version = version
	return s.result, s.err
}

func (s *installReleaseStub) Create(
	context.Context,
	releasedomain.CreateDraftRequest,
) (releasedomain.Release, error) {
	return releasedomain.Release{}, nil
}

func (s *installReleaseStub) List(
	context.Context,
	identity.Actor,
	string,
	string,
	releasedomain.ReleaseListRequest,
) (releasedomain.ReleasePage, error) {
	return releasedomain.ReleasePage{}, nil
}

func (s *installReleaseStub) AddArtifact(
	context.Context,
	releasedomain.AddArtifactRequest,
) (releasedomain.ReleaseArtifact, error) {
	return releasedomain.ReleaseArtifact{}, nil
}

func (s *installReleaseStub) RemoveArtifact(
	context.Context,
	releasedomain.RemoveArtifactRequest,
) (releasedomain.RemoveArtifactResult, error) {
	return releasedomain.RemoveArtifactResult{}, nil
}

func (s *installReleaseStub) Cancel(
	context.Context,
	releasedomain.CancelDraftRequest,
) (releasedomain.CancelDraftResult, error) {
	return releasedomain.CancelDraftResult{}, nil
}

func (s *installArtifactStub) Upload(context.Context, artifactdomain.UploadRequest) (artifactdomain.Metadata, error) {
	return artifactdomain.Metadata{}, nil
}

func (s *installArtifactStub) ChecksumDeploy(context.Context, artifactdomain.ChecksumDeployRequest) (artifactdomain.Metadata, error) {
	return artifactdomain.Metadata{}, nil
}

func (s *installArtifactStub) Metadata(context.Context, identity.Actor, string, string) (artifactdomain.Metadata, error) {
	return artifactdomain.Metadata{}, nil
}

func (s *installArtifactStub) Open(_ context.Context, request artifactdomain.OpenRequest) (artifactdomain.OpenResult, error) {
	s.calls++
	s.request = request
	return s.result, s.err
}
