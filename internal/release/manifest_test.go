package release

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestBuildManifestIsByteStableAcrossInputOrder(t *testing.T) {
	publishedAt := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	arm := SnapshotArtifact{
		Path: "linux/arm64/edgecli", Filename: "edgecli", OS: "linux", Arch: "arm64",
		Role: "binary", MediaType: "application/octet-stream",
		SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Size: 7,
	}
	amd := SnapshotArtifact{
		Path: "linux/amd64/edgecli", Filename: "edgecli", OS: "linux", Arch: "amd64",
		Role: "binary", MediaType: "application/octet-stream",
		SHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Size: 9,
	}
	first, err := BuildManifest(PublishSnapshot{
		Repository: "releases", Package: "edgecli", Version: "1.2.3", PublishedAt: publishedAt,
		Artifacts: []SnapshotArtifact{arm, amd},
	})
	if err != nil {
		t.Fatalf("BuildManifest(first) error = %v", err)
	}
	second, err := BuildManifest(PublishSnapshot{
		Repository: "releases", Package: "edgecli", Version: "1.2.3", PublishedAt: publishedAt,
		Artifacts: []SnapshotArtifact{amd, arm},
	})
	if err != nil {
		t.Fatalf("BuildManifest(second) error = %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("manifest bytes differ:\n%s\n%s", first, second)
	}
	want := `{"artifacts":[{"arch":"amd64","filename":"edgecli","mediaType":"application/octet-stream","os":"linux","path":"linux/amd64/edgecli","role":"binary","sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","size":9,"variant":""},{"arch":"arm64","filename":"edgecli","mediaType":"application/octet-stream","os":"linux","path":"linux/arm64/edgecli","role":"binary","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","size":7,"variant":""}],"package":"edgecli","publishedAt":"2026-07-11T12:00:00Z","repository":"releases","schemaVersion":1,"version":"1.2.3"}`
	if string(first) != want {
		t.Fatalf("manifest = %s\nwant     = %s", first, want)
	}
}

func TestBuildManifestRejectsDuplicateCoordinates(t *testing.T) {
	artifact := SnapshotArtifact{
		Path: "linux/arm64/edgecli", Filename: "edgecli", OS: "linux", Arch: "arm64",
		SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Size: 7,
		MediaType: "application/octet-stream",
	}
	_, err := BuildManifest(PublishSnapshot{
		Repository: "releases", Package: "edgecli", Version: "1.2.3", PublishedAt: time.Now().UTC(),
		Artifacts: []SnapshotArtifact{artifact, artifact},
	})
	if err == nil {
		t.Fatal("BuildManifest() accepted duplicate platform coordinates")
	}
}

func TestBuildManifestV2IncludesProductAndInstallPlan(t *testing.T) {
	t.Parallel()
	manifest, err := BuildManifest(PublishSnapshot{
		Repository:  "cli-releases",
		Package:     "edgectl",
		Product:     &SnapshotProduct{Slug: "edgectl", Command: "edgectl"},
		Version:     "1.2.3",
		PublishedAt: time.Date(2026, 7, 23, 9, 0, 0, 0, time.UTC),
		Artifacts: []SnapshotArtifact{{
			Path: "edgectl/1.2.3/linux/arm64/edgectl", Filename: "edgectl",
			OS: "linux", Arch: "arm64", Role: "binary", MediaType: "application/octet-stream",
			SHA256: strings.Repeat("a", 64), Size: 42,
			Install: &InstallSpec{Strategy: InstallStrategySelfReplace, Format: InstallFormatRaw, Mode: "0755"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(manifest, &document); err != nil {
		t.Fatal(err)
	}
	if document["schemaVersion"] != float64(2) {
		t.Fatalf("schemaVersion = %#v, want 2", document["schemaVersion"])
	}
	product, ok := document["product"].(map[string]any)
	if !ok || product["slug"] != "edgectl" || product["command"] != "edgectl" {
		t.Fatalf("product = %#v", document["product"])
	}
}

func TestBuildManifestV2RequiresInstallSpecForEveryArtifact(t *testing.T) {
	t.Parallel()
	_, err := BuildManifest(PublishSnapshot{
		Repository:  "cli-releases",
		Package:     "edgectl",
		Product:     &SnapshotProduct{Slug: "edgectl", Command: "edgectl"},
		Version:     "1.2.3",
		PublishedAt: time.Now(),
		Artifacts: []SnapshotArtifact{{
			Path: "edgectl", Filename: "edgectl", OS: "linux", Arch: "arm64", Role: "binary",
			MediaType: "application/octet-stream", SHA256: strings.Repeat("a", 64), Size: 1,
		}},
	})
	if !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("BuildManifest() error = %v, want ErrInvalidSnapshot", err)
	}
}

func TestBuildManifestV2RequiresBinaryRoleForEveryArtifact(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name string
		role string
	}{
		{name: "missing", role: ""},
		{name: "non-binary", role: "metadata"},
	} {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, err := BuildManifest(PublishSnapshot{
				Repository:  "cli-releases",
				Package:     "edgectl",
				Product:     &SnapshotProduct{Slug: "edgectl", Command: "edgectl"},
				Version:     "1.2.3",
				PublishedAt: time.Now(),
				Artifacts: []SnapshotArtifact{{
					Path: "edgectl", Filename: "edgectl", OS: "linux", Arch: "arm64", Role: testCase.role,
					MediaType: "application/octet-stream", SHA256: strings.Repeat("a", 64), Size: 1,
					Install: &InstallSpec{Strategy: InstallStrategySelfReplace, Format: InstallFormatRaw, Mode: "0755"},
				}},
			})
			if !errors.Is(err, ErrInvalidSnapshot) {
				t.Fatalf("BuildManifest() role %q error = %v, want ErrInvalidSnapshot", testCase.role, err)
			}
		})
	}
}

func TestBuildManifestV2RejectsInvalidInstallSpec(t *testing.T) {
	t.Parallel()
	_, err := BuildManifest(PublishSnapshot{
		Repository:  "cli-releases",
		Package:     "edgectl",
		Product:     &SnapshotProduct{Slug: "edgectl", Command: "edgectl"},
		Version:     "1.2.3",
		PublishedAt: time.Now(),
		Artifacts: []SnapshotArtifact{{
			Path: "edgectl", Filename: "edgectl", OS: "linux", Arch: "arm64", Role: "binary",
			MediaType: "application/octet-stream", SHA256: strings.Repeat("a", 64), Size: 1,
			Install: &InstallSpec{Strategy: InstallStrategySelfReplace, Format: InstallFormatRaw, Mode: "0644"},
		}},
	})
	if !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("BuildManifest() error = %v, want ErrInvalidSnapshot", err)
	}
}

func TestBuildManifestV1KeepsNonProductRoles(t *testing.T) {
	t.Parallel()
	manifest, err := BuildManifest(PublishSnapshot{
		Repository:  "releases",
		Package:     "metadata",
		Version:     "1.2.3",
		PublishedAt: time.Date(2026, 7, 23, 9, 0, 0, 0, time.UTC),
		Artifacts: []SnapshotArtifact{{
			Path: "metadata.json", Filename: "metadata.json", OS: "any", Arch: "any", Role: "metadata",
			MediaType: "application/json", SHA256: strings.Repeat("a", 64), Size: 1,
		}},
	})
	if err != nil {
		t.Fatalf("BuildManifest() rejected a legacy non-product role: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(manifest, &document); err != nil {
		t.Fatal(err)
	}
	if document["schemaVersion"] != float64(1) {
		t.Fatalf("schemaVersion = %#v, want 1", document["schemaVersion"])
	}
}

func TestBuildManifestV2AcceptsProductCommandNameContract(t *testing.T) {
	t.Parallel()
	_, err := BuildManifest(PublishSnapshot{
		Repository:  "cli-releases",
		Package:     "x-cli",
		Product:     &SnapshotProduct{Slug: "x-cli", Command: "X"},
		Version:     "1.2.3",
		PublishedAt: time.Now(),
		Artifacts: []SnapshotArtifact{{
			Path: "x-cli", Filename: "X", OS: "linux", Arch: "amd64", Role: "binary",
			MediaType: "application/octet-stream", SHA256: strings.Repeat("a", 64), Size: 1,
			Install: &InstallSpec{Strategy: InstallStrategySelfReplace, Format: InstallFormatRaw, Mode: "0755"},
		}},
	})
	if err != nil {
		t.Fatalf("BuildManifest() rejected a valid command name: %v", err)
	}
}
