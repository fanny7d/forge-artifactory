package forgeupdate

import (
	"bytes"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"testing"
)

func TestVerifierAcceptsStrictV1AndBindsCoordinates(t *testing.T) {
	encoded := []byte(`{
		"schemaVersion":1,
		"repository":"tools",
		"package":"edgecli",
		"version":"1.2.0",
		"publishedAt":"2026-07-23T00:00:00Z",
		"artifacts":[{
			"path":"linux/arm64/edgecli",
			"filename":"edgecli",
			"os":"linux",
			"arch":"arm64",
			"variant":"",
			"role":"binary",
			"mediaType":"application/octet-stream",
			"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"size":42
		}]
	}`)
	signed, verifier := signForTest(t, encoded)
	release, err := verifier.Verify(signed, Selection{
		Repository: "tools", Package: "edgecli", CurrentVersion: "1.1.9",
		OS: "linux", Arch: "arm64", Role: "binary",
	})
	if err != nil {
		t.Fatal(err)
	}
	if release.manifest.SchemaVersion != 1 || release.artifact.Kind != ArtifactBinary ||
		release.artifact.Path != "linux/arm64/edgecli" {
		t.Fatalf("verified release = %+v", release)
	}
}

func TestVerifierAcceptsV2BundleAndSignedHooks(t *testing.T) {
	encoded := []byte(`{
		"schemaVersion":2,
		"repository":"tools",
		"package":"edgecli",
		"product":{"slug":"edgecli","command":"edgecli"},
		"version":"2.0.0-beta.1",
		"publishedAt":"2026-07-23T00:00:00Z",
		"artifacts":[{
			"path":"bundles/linux-arm64.tar.gz",
			"filename":"linux-arm64.tar.gz",
			"os":"linux",
			"arch":"arm64",
			"variant":"",
			"role":"binary",
			"mediaType":"application/gzip",
			"sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			"size":100,
			"install":{
				"strategy":"bundle",
				"format":"tar.gz",
				"entrypoint":"bin/edgecli",
				"mode":"0755",
				"unpackedSize":200,
				"fileCount":2,
				"hooks":[{"phase":"preflight","path":"bin/check","args":["--version"],"timeoutSeconds":5}]
			}
		}]
	}`)
	signed, verifier := signForTest(t, encoded)
	release, err := verifier.Verify(signed, Selection{
		Repository: "tools", Package: "edgecli", CurrentVersion: "1.9.9",
		OS: "linux", Arch: "arm64", Role: "binary",
	})
	if err != nil {
		t.Fatal(err)
	}
	if release.artifact.Kind != ArtifactBundle ||
		release.artifact.Strategy != InstallStrategyBundle ||
		release.artifact.Format != ArchiveTarGz || release.artifact.Mode != "0755" ||
		len(release.artifact.Hooks) != 1 || release.artifact.Hooks[0].Args[0] != "--version" {
		t.Fatalf("verified artifact = %+v", release.artifact)
	}
	exposed := release.Artifact()
	exposed.Hooks[0].Args[0] = "--tampered"
	if release.Artifact().Hooks[0].Args[0] != "--version" {
		t.Fatal("Artifact() exposed mutable verified hook state")
	}
}

func TestVerifierV2BindsProductSlug(t *testing.T) {
	encoded := []byte(`{
		"schemaVersion":2,
		"repository":"tools",
		"package":"edgecli",
		"product":{"slug":"othercli","command":"othercli"},
		"version":"2.0.0",
		"publishedAt":"2026-07-23T00:00:00Z",
		"artifacts":[{
			"path":"linux/arm64/edgecli",
			"filename":"edgecli",
			"os":"linux",
			"arch":"arm64",
			"variant":"",
			"role":"binary",
			"mediaType":"application/octet-stream",
			"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"size":42,
			"install":{"strategy":"self-replace","format":"raw","mode":"0755"}
		}]
	}`)
	signed, verifier := signForTest(t, encoded)
	if _, err := verifier.Verify(signed, Selection{
		Repository: "tools", Package: "edgecli", CurrentVersion: "1.0.0",
		OS: "linux", Arch: "arm64", Role: "binary",
	}); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("Verify() product mismatch error = %v", err)
	}
}

func TestParseManifestV2UsesDistinctProductSlugAndCommandContracts(t *testing.T) {
	encoded := []byte(`{
		"schemaVersion":2,
		"repository":"tools",
		"package":"edgecli",
		"product":{"slug":"edgecli","command":"E"},
		"version":"2.0.0",
		"publishedAt":"2026-07-23T00:00:00Z",
		"artifacts":[{
			"path":"linux/arm64/edgecli",
			"filename":"edgecli",
			"os":"linux",
			"arch":"arm64",
			"variant":"",
			"role":"binary",
			"mediaType":"application/octet-stream",
			"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"size":42,
			"install":{"strategy":"self-replace","format":"raw","mode":"0755"}
		}]
	}`)
	manifest, err := ParseManifest(encoded)
	if err != nil {
		t.Fatalf("ParseManifest() rejected valid one-character uppercase command: %v", err)
	}
	if manifest.Product == nil || manifest.Product.Command != "E" {
		t.Fatalf("product = %+v", manifest.Product)
	}

	invalid := bytes.Replace(encoded, []byte(`"command":"E"`), []byte(`"command":"bad/name"`), 1)
	if _, err := ParseManifest(invalid); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("ParseManifest() invalid command error = %v", err)
	}
}

func TestVerifierRejectsTamperingUnknownFieldsAndNoUpdate(t *testing.T) {
	valid := map[string]any{
		"schemaVersion": 1,
		"repository":    "tools",
		"package":       "edgecli",
		"version":       "1.2.0",
		"publishedAt":   "2026-07-23T00:00:00Z",
		"artifacts": []any{map[string]any{
			"path": "linux/arm64/edgecli", "filename": "edgecli",
			"os": "linux", "arch": "arm64", "variant": "", "role": "binary",
			"mediaType": "application/octet-stream",
			"sha256":    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"size":      42,
		}},
	}
	encoded, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	signed, verifier := signForTest(t, encoded)

	tampered := signed
	tampered.Manifest = append([]byte(nil), signed.Manifest...)
	tampered.Manifest[len(tampered.Manifest)-1] ^= 1
	if _, err := verifier.Verify(tampered, Selection{}); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("tampered Verify() error = %v", err)
	}

	withUnknown := cloneJSONMap(t, valid)
	withUnknown["artifacts"].([]any)[0].(map[string]any)["kind"] = "binary"
	unknownBytes, err := json.Marshal(withUnknown)
	if err != nil {
		t.Fatal(err)
	}
	unknownSigned, unknownVerifier := signForTest(t, unknownBytes)
	if _, err := unknownVerifier.Verify(unknownSigned, Selection{}); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("unknown field Verify() error = %v", err)
	}

	if _, err := verifier.Verify(signed, Selection{
		Repository: "tools", Package: "edgecli", CurrentVersion: "1.2.0",
		OS: "linux", Arch: "arm64", Role: "binary",
	}); !errors.Is(err, ErrNoUpdate) {
		t.Fatalf("no-update Verify() error = %v", err)
	}
}

func TestParseManifestRejectsDuplicatePlatformCoordinate(t *testing.T) {
	encoded := []byte(`{
		"schemaVersion":1,
		"repository":"tools",
		"package":"edgecli",
		"version":"1.2.0",
		"publishedAt":"2026-07-23T00:00:00Z",
		"artifacts":[
			{"path":"a/edgecli","filename":"edgecli","os":"linux","arch":"arm64","variant":"","role":"binary","mediaType":"application/octet-stream","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","size":1},
			{"path":"b/edgecli","filename":"edgecli","os":"linux","arch":"arm64","variant":"","role":"binary","mediaType":"application/octet-stream","sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","size":1}
		]
	}`)
	if _, err := ParseManifest(encoded); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("ParseManifest() error = %v", err)
	}
}

func TestParseManifestRejectsDuplicateJSONField(t *testing.T) {
	encoded := []byte(`{
		"schemaVersion":1,
		"schemaVersion":2,
		"repository":"tools",
		"package":"edgecli",
		"version":"1.2.0",
		"publishedAt":"2026-07-23T00:00:00Z",
		"artifacts":[]
	}`)
	if _, err := ParseManifest(encoded); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("ParseManifest() error = %v", err)
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		left  string
		right string
		want  int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.1", "1.0.0", 1},
		{"2.0.0", "10.0.0", -1},
		{"1.0.0-alpha", "1.0.0", -1},
		{"1.0.0-alpha.2", "1.0.0-alpha.10", -1},
		{"1.0.0+build.1", "1.0.0+build.2", 0},
	}
	for _, test := range tests {
		got, err := CompareVersions(test.left, test.right)
		if err != nil {
			t.Fatalf("CompareVersions(%q, %q): %v", test.left, test.right, err)
		}
		if got != test.want {
			t.Fatalf("CompareVersions(%q, %q) = %d, want %d", test.left, test.right, got, test.want)
		}
	}
	for _, invalid := range []string{"v1.0.0", "1.0", "1.0.0-01", "1.0.0+", "1.0.0 α"} {
		if _, err := CompareVersions(invalid, "1.0.0"); err == nil {
			t.Fatalf("CompareVersions accepted %q", invalid)
		}
	}
}

func signForTest(t *testing.T, encoded []byte) (SignedManifest, Verifier) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyID := KeyID(publicKey)
	return SignedManifest{
		KeyID: keyID, Manifest: encoded, Signature: ed25519.Sign(privateKey, encoded),
	}, Verifier{TrustedKeys: map[string]ed25519.PublicKey{keyID: publicKey}}
}

func cloneJSONMap(t *testing.T, source map[string]any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	var clone map[string]any
	if err := json.Unmarshal(encoded, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}
