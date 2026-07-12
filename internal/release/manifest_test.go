package release

import (
	"bytes"
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
