package release

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/gowebpki/jcs"
)

var (
	ErrInvalidSnapshot = errors.New("invalid publish snapshot")
	manifestSHA256     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	manifestSlug       = regexp.MustCompile(`^[a-z][a-z0-9._-]{1,63}$`)
	manifestCommand    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

type PublishSnapshot struct {
	Repository  string             `json:"repository"`
	Package     string             `json:"package"`
	Product     *SnapshotProduct   `json:"product,omitempty"`
	Version     string             `json:"version"`
	PublishedAt time.Time          `json:"publishedAt"`
	Artifacts   []SnapshotArtifact `json:"artifacts"`
}

type SnapshotProduct struct {
	Slug    string `json:"slug"`
	Command string `json:"command"`
}

type SnapshotArtifact struct {
	Path      string       `json:"path"`
	Filename  string       `json:"filename"`
	OS        string       `json:"os"`
	Arch      string       `json:"arch"`
	Variant   string       `json:"variant"`
	Role      string       `json:"role"`
	MediaType string       `json:"mediaType"`
	SHA256    string       `json:"sha256"`
	Size      int64        `json:"size"`
	Install   *InstallSpec `json:"install,omitempty"`
}

type manifestDocument struct {
	SchemaVersion int                `json:"schemaVersion"`
	Repository    string             `json:"repository"`
	Package       string             `json:"package"`
	Product       *SnapshotProduct   `json:"product,omitempty"`
	Version       string             `json:"version"`
	PublishedAt   time.Time          `json:"publishedAt"`
	Artifacts     []SnapshotArtifact `json:"artifacts"`
}

func BuildManifest(snapshot PublishSnapshot) ([]byte, error) {
	if snapshot.Repository == "" || snapshot.Package == "" || snapshot.Version == "" ||
		snapshot.PublishedAt.IsZero() || len(snapshot.Artifacts) == 0 {
		return nil, ErrInvalidSnapshot
	}
	artifacts := append([]SnapshotArtifact(nil), snapshot.Artifacts...)
	schemaVersion := 1
	if snapshot.Product != nil {
		schemaVersion = 2
		if !manifestSlug.MatchString(snapshot.Product.Slug) || !manifestCommand.MatchString(snapshot.Product.Command) {
			return nil, ErrInvalidSnapshot
		}
	}
	coordinates := make(map[artifactCoordinate]struct{}, len(artifacts))
	for _, artifact := range artifacts {
		if artifact.Path == "" || artifact.Filename == "" || artifact.OS == "" || artifact.Arch == "" ||
			artifact.MediaType == "" || !manifestSHA256.MatchString(artifact.SHA256) || artifact.Size < 0 {
			return nil, ErrInvalidSnapshot
		}
		if artifact.Install != nil {
			schemaVersion = 2
			if err := artifact.Install.Validate(); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrInvalidSnapshot, err)
			}
		}
		coordinate := artifactCoordinate{
			OS: artifact.OS, Arch: artifact.Arch, Variant: artifact.Variant, Role: artifact.Role,
		}
		if _, duplicate := coordinates[coordinate]; duplicate {
			return nil, fmt.Errorf("%w: duplicate artifact coordinate", ErrInvalidSnapshot)
		}
		coordinates[coordinate] = struct{}{}
	}
	sort.Slice(artifacts, func(left, right int) bool {
		a := artifacts[left]
		b := artifacts[right]
		if a.OS != b.OS {
			return a.OS < b.OS
		}
		if a.Arch != b.Arch {
			return a.Arch < b.Arch
		}
		if a.Variant != b.Variant {
			return a.Variant < b.Variant
		}
		if a.Role != b.Role {
			return a.Role < b.Role
		}
		return a.Path < b.Path
	})
	if schemaVersion == 2 {
		if snapshot.Product == nil {
			return nil, fmt.Errorf("%w: product metadata is required for installable artifacts", ErrInvalidSnapshot)
		}
		for _, artifact := range artifacts {
			if artifact.Role != "binary" {
				return nil, fmt.Errorf("%w: every product artifact requires role binary", ErrInvalidSnapshot)
			}
			if artifact.Install == nil {
				return nil, fmt.Errorf("%w: every product artifact requires an install spec", ErrInvalidSnapshot)
			}
		}
	}
	document := manifestDocument{
		SchemaVersion: schemaVersion,
		Repository:    snapshot.Repository,
		Package:       snapshot.Package,
		Product:       snapshot.Product,
		Version:       snapshot.Version,
		PublishedAt:   snapshot.PublishedAt.UTC(),
		Artifacts:     artifacts,
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode manifest: %w", err)
	}
	canonical, err := jcs.Transform(encoded)
	if err != nil {
		return nil, fmt.Errorf("canonicalize manifest: %w", err)
	}
	return canonical, nil
}

type artifactCoordinate struct {
	OS      string
	Arch    string
	Variant string
	Role    string
}
