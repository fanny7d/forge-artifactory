package forgeupdate

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const DefaultMaxManifestBytes = 1 << 20

type ArtifactKind string

const (
	ArtifactBinary ArtifactKind = "binary"
	ArtifactBundle ArtifactKind = "bundle"
)

type ArchiveFormat string

const (
	ArchiveRaw   ArchiveFormat = "raw"
	ArchiveTarGz ArchiveFormat = "tar.gz"
	ArchiveZIP   ArchiveFormat = "zip"
)

type InstallStrategy string

const (
	InstallStrategySelfReplace InstallStrategy = "self-replace"
	InstallStrategyBundle      InstallStrategy = "bundle"
)

type HookPhase string

const (
	HookPreflight   HookPhase = "preflight"
	HookPostInstall HookPhase = "post-install"
	HookVerify      HookPhase = "verify"
)

type Hook struct {
	Phase          HookPhase `json:"phase"`
	Path           string    `json:"path"`
	Args           []string  `json:"args"`
	TimeoutSeconds int       `json:"timeoutSeconds"`
}

type Product struct {
	Slug    string
	Command string
}

type Artifact struct {
	Path         string
	Filename     string
	OS           string
	Arch         string
	Variant      string
	Role         string
	MediaType    string
	SHA256       string
	Size         int64
	Kind         ArtifactKind
	Strategy     InstallStrategy
	Format       ArchiveFormat
	Entrypoint   string
	Mode         string
	UnpackedSize int64
	FileCount    int
	Hooks        []Hook
}

type Manifest struct {
	SchemaVersion int
	Repository    string
	Package       string
	Product       *Product
	Version       string
	PublishedAt   time.Time
	Artifacts     []Artifact
}

type SignedManifest struct {
	KeyID     string
	Manifest  []byte
	Signature []byte
}

type Selection struct {
	Repository     string
	Package        string
	CurrentVersion string
	OS             string
	Arch           string
	Variant        string
	Role           string
}

type VerifiedRelease struct {
	manifest Manifest
	artifact Artifact
}

func (release VerifiedRelease) Manifest() Manifest {
	return cloneManifest(release.manifest)
}

func (release VerifiedRelease) Artifact() Artifact {
	return cloneArtifact(release.artifact)
}

type Verifier struct {
	TrustedKeys      map[string]ed25519.PublicKey
	MaxManifestBytes int
}

type manifestEnvelope struct {
	SchemaVersion int `json:"schemaVersion"`
}

type manifestV1 struct {
	SchemaVersion int          `json:"schemaVersion"`
	Repository    string       `json:"repository"`
	Package       string       `json:"package"`
	Version       string       `json:"version"`
	PublishedAt   time.Time    `json:"publishedAt"`
	Artifacts     []artifactV1 `json:"artifacts"`
}

type artifactV1 struct {
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

type manifestV2 struct {
	SchemaVersion int          `json:"schemaVersion"`
	Repository    string       `json:"repository"`
	Package       string       `json:"package"`
	Product       productV2    `json:"product"`
	Version       string       `json:"version"`
	PublishedAt   time.Time    `json:"publishedAt"`
	Artifacts     []artifactV2 `json:"artifacts"`
}

type productV2 struct {
	Slug    string `json:"slug"`
	Command string `json:"command"`
}

type artifactV2 struct {
	Path      string    `json:"path"`
	Filename  string    `json:"filename"`
	OS        string    `json:"os"`
	Arch      string    `json:"arch"`
	Variant   string    `json:"variant"`
	Role      string    `json:"role"`
	MediaType string    `json:"mediaType"`
	SHA256    string    `json:"sha256"`
	Size      int64     `json:"size"`
	Install   installV2 `json:"install"`
}

type installV2 struct {
	Strategy     InstallStrategy `json:"strategy"`
	Format       ArchiveFormat   `json:"format"`
	Entrypoint   string          `json:"entrypoint,omitempty"`
	Mode         string          `json:"mode"`
	Hooks        []Hook          `json:"hooks,omitempty"`
	UnpackedSize int64           `json:"unpackedSize,omitempty"`
	FileCount    int             `json:"fileCount,omitempty"`
}

var (
	validProductSlug    = regexp.MustCompile(`^[a-z][a-z0-9._-]{1,63}$`)
	validProductCommand = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// KeyID returns the stable identifier expected in signed Forge manifests.
func KeyID(publicKey ed25519.PublicKey) string {
	fingerprint := sha256.Sum256(publicKey)
	return "ed25519:" + hex.EncodeToString(fingerprint[:])
}

// Verify verifies the exact manifest bytes before parsing them, strictly
// parses schema v1 or v2, binds package/platform coordinates, and rejects a
// candidate that is not newer than CurrentVersion.
func (verifier Verifier) Verify(signed SignedManifest, selection Selection) (VerifiedRelease, error) {
	maximum := verifier.MaxManifestBytes
	if maximum <= 0 {
		maximum = DefaultMaxManifestBytes
	}
	if len(signed.Manifest) == 0 || len(signed.Manifest) > maximum {
		return VerifiedRelease{}, fmt.Errorf("%w: manifest size %d exceeds limit %d", ErrInvalidManifest, len(signed.Manifest), maximum)
	}
	publicKey, ok := verifier.TrustedKeys[signed.KeyID]
	if !ok {
		return VerifiedRelease{}, fmt.Errorf("%w: %q", ErrUntrustedKey, signed.KeyID)
	}
	if len(publicKey) != ed25519.PublicKeySize || KeyID(publicKey) != signed.KeyID {
		return VerifiedRelease{}, fmt.Errorf("%w: key ID does not match key material", ErrUntrustedKey)
	}
	if len(signed.Signature) != ed25519.SignatureSize ||
		!ed25519.Verify(publicKey, signed.Manifest, signed.Signature) {
		return VerifiedRelease{}, ErrInvalidSignature
	}
	manifest, err := ParseManifest(signed.Manifest)
	if err != nil {
		return VerifiedRelease{}, err
	}
	if manifest.Repository != selection.Repository || manifest.Package != selection.Package {
		return VerifiedRelease{}, fmt.Errorf("%w: package identity does not match selection", ErrInvalidManifest)
	}
	if manifest.SchemaVersion == 2 &&
		(manifest.Product == nil || manifest.Product.Slug != selection.Package) {
		return VerifiedRelease{}, fmt.Errorf("%w: product slug does not match selection", ErrInvalidManifest)
	}
	if selection.CurrentVersion != "" {
		comparison, compareErr := CompareVersions(manifest.Version, selection.CurrentVersion)
		if compareErr != nil {
			return VerifiedRelease{}, fmt.Errorf("%w: compare versions: %v", ErrInvalidManifest, compareErr)
		}
		if comparison <= 0 {
			return VerifiedRelease{}, fmt.Errorf("%w: candidate %s is not newer than %s", ErrNoUpdate, manifest.Version, selection.CurrentVersion)
		}
	}
	var selected *Artifact
	for index := range manifest.Artifacts {
		artifact := &manifest.Artifacts[index]
		if artifact.OS == selection.OS && artifact.Arch == selection.Arch &&
			artifact.Variant == selection.Variant && artifact.Role == selection.Role {
			if selected != nil {
				return VerifiedRelease{}, fmt.Errorf("%w: duplicate matching artifact coordinate", ErrInvalidManifest)
			}
			selected = artifact
		}
	}
	if selected == nil {
		return VerifiedRelease{}, fmt.Errorf("%w: selected platform artifact is absent", ErrInvalidManifest)
	}
	return VerifiedRelease{manifest: cloneManifest(manifest), artifact: cloneArtifact(*selected)}, nil
}

// ParseManifest strictly parses either manifest schema v1 or schema v2.
func ParseManifest(encoded []byte) (Manifest, error) {
	if err := rejectDuplicateJSONFields(encoded); err != nil {
		return Manifest{}, fmt.Errorf("%w: %v", ErrInvalidManifest, err)
	}
	var envelope manifestEnvelope
	if err := decodeStrictJSON(encoded, &envelope, false); err != nil {
		return Manifest{}, fmt.Errorf("%w: decode schema version: %v", ErrInvalidManifest, err)
	}
	var manifest Manifest
	switch envelope.SchemaVersion {
	case 1:
		var source manifestV1
		if err := decodeStrictJSON(encoded, &source, true); err != nil {
			return Manifest{}, fmt.Errorf("%w: decode v1: %v", ErrInvalidManifest, err)
		}
		manifest = Manifest{
			SchemaVersion: source.SchemaVersion,
			Repository:    source.Repository,
			Package:       source.Package,
			Version:       source.Version,
			PublishedAt:   source.PublishedAt,
			Artifacts:     make([]Artifact, 0, len(source.Artifacts)),
		}
		for _, artifact := range source.Artifacts {
			manifest.Artifacts = append(manifest.Artifacts, Artifact{
				Path: artifact.Path, Filename: artifact.Filename,
				OS: artifact.OS, Arch: artifact.Arch, Variant: artifact.Variant,
				Role: artifact.Role, MediaType: artifact.MediaType,
				SHA256: artifact.SHA256, Size: artifact.Size, Kind: ArtifactBinary,
			})
		}
	case 2:
		var source manifestV2
		if err := decodeStrictJSON(encoded, &source, true); err != nil {
			return Manifest{}, fmt.Errorf("%w: decode v2: %v", ErrInvalidManifest, err)
		}
		manifest = Manifest{
			SchemaVersion: source.SchemaVersion,
			Repository:    source.Repository,
			Package:       source.Package,
			Product:       &Product{Slug: source.Product.Slug, Command: source.Product.Command},
			Version:       source.Version,
			PublishedAt:   source.PublishedAt,
			Artifacts:     make([]Artifact, 0, len(source.Artifacts)),
		}
		for _, artifact := range source.Artifacts {
			kind := ArtifactKind("")
			switch artifact.Install.Strategy {
			case InstallStrategySelfReplace:
				kind = ArtifactBinary
			case InstallStrategyBundle:
				kind = ArtifactBundle
			}
			manifest.Artifacts = append(manifest.Artifacts, Artifact{
				Path: artifact.Path, Filename: artifact.Filename,
				OS: artifact.OS, Arch: artifact.Arch, Variant: artifact.Variant,
				Role: artifact.Role, MediaType: artifact.MediaType,
				SHA256: artifact.SHA256, Size: artifact.Size, Kind: kind,
				Strategy: artifact.Install.Strategy, Format: artifact.Install.Format,
				Entrypoint: artifact.Install.Entrypoint, Mode: artifact.Install.Mode,
				UnpackedSize: artifact.Install.UnpackedSize, FileCount: artifact.Install.FileCount,
				Hooks: append([]Hook(nil), artifact.Install.Hooks...),
			})
		}
	default:
		return Manifest{}, fmt.Errorf("%w: unsupported schema version %d", ErrInvalidManifest, envelope.SchemaVersion)
	}
	if err := validateManifest(manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func rejectDuplicateJSONFields(encoded []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var readValue func() error
	readValue = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, isDelimiter := token.(json.Delim)
		if !isDelimiter {
			return nil
		}
		switch delimiter {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return fmt.Errorf("object key is not a string")
				}
				if _, duplicate := seen[key]; duplicate {
					return fmt.Errorf("duplicate JSON field %q", key)
				}
				seen[key] = struct{}{}
				if err := readValue(); err != nil {
					return err
				}
			}
			closing, err := decoder.Token()
			if err != nil {
				return err
			}
			if closing != json.Delim('}') {
				return fmt.Errorf("object is not closed")
			}
		case '[':
			for decoder.More() {
				if err := readValue(); err != nil {
					return err
				}
			}
			closing, err := decoder.Token()
			if err != nil {
				return err
			}
			if closing != json.Delim(']') {
				return fmt.Errorf("array is not closed")
			}
		default:
			return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
		}
		return nil
	}
	if err := readValue(); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("document must contain exactly one JSON value")
		}
		return err
	}
	return nil
}

func decodeStrictJSON(encoded []byte, destination any, rejectUnknown bool) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	if rejectUnknown {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("document must contain exactly one JSON value")
		}
		return fmt.Errorf("read trailing data: %w", err)
	}
	return nil
}

func validateManifest(manifest Manifest) error {
	if manifest.Repository == "" || manifest.Package == "" || manifest.PublishedAt.IsZero() ||
		len(manifest.Artifacts) == 0 {
		return fmt.Errorf("%w: required manifest field is empty", ErrInvalidManifest)
	}
	if _, err := parseSemanticVersion(manifest.Version); err != nil {
		return fmt.Errorf("%w: invalid version: %v", ErrInvalidManifest, err)
	}
	switch manifest.SchemaVersion {
	case 1:
		if manifest.Product != nil {
			return fmt.Errorf("%w: v1 manifest contains product metadata", ErrInvalidManifest)
		}
	case 2:
		if manifest.Product == nil ||
			!validProductSlug.MatchString(manifest.Product.Slug) ||
			!validProductCommand.MatchString(manifest.Product.Command) {
			return fmt.Errorf("%w: invalid product metadata", ErrInvalidManifest)
		}
	default:
		return fmt.Errorf("%w: unsupported schema version %d", ErrInvalidManifest, manifest.SchemaVersion)
	}
	coordinates := make(map[string]struct{}, len(manifest.Artifacts))
	for index := range manifest.Artifacts {
		artifact := &manifest.Artifacts[index]
		if err := validateArtifact(manifest.SchemaVersion, artifact); err != nil {
			return fmt.Errorf("%w: artifact %d: %v", ErrInvalidManifest, index, err)
		}
		coordinate := strings.Join([]string{artifact.OS, artifact.Arch, artifact.Variant, artifact.Role}, "\x00")
		if _, duplicate := coordinates[coordinate]; duplicate {
			return fmt.Errorf("%w: duplicate artifact coordinate", ErrInvalidManifest)
		}
		coordinates[coordinate] = struct{}{}
	}
	return nil
}

func validateArtifact(schemaVersion int, artifact *Artifact) error {
	if artifact.Path == "" || strings.HasPrefix(artifact.Path, "/") ||
		strings.Contains(artifact.Path, "\\") || path.Clean(artifact.Path) != artifact.Path ||
		artifact.Filename == "" || path.Base(artifact.Path) != artifact.Filename ||
		artifact.OS == "" || artifact.Arch == "" || artifact.MediaType == "" {
		return fmt.Errorf("required artifact field is empty or invalid")
	}
	if len(artifact.SHA256) != sha256.Size*2 || strings.ToLower(artifact.SHA256) != artifact.SHA256 {
		return fmt.Errorf("SHA-256 must be 64 lowercase hexadecimal characters")
	}
	if _, err := hex.DecodeString(artifact.SHA256); err != nil {
		return fmt.Errorf("invalid SHA-256: %w", err)
	}
	if artifact.Size < 0 {
		return fmt.Errorf("size must not be negative")
	}
	if schemaVersion == 1 {
		if artifact.Kind != ArtifactBinary || artifact.Strategy != "" || artifact.Format != "" ||
			artifact.Entrypoint != "" || artifact.Mode != "" ||
			artifact.UnpackedSize != 0 || artifact.FileCount != 0 || len(artifact.Hooks) != 0 {
			return fmt.Errorf("v1 artifact contains v2-only fields")
		}
		return nil
	}
	if err := validateInstallMode(artifact.Mode); err != nil {
		return err
	}
	switch artifact.Kind {
	case ArtifactBinary:
		if artifact.Strategy != InstallStrategySelfReplace || artifact.Format != ArchiveRaw ||
			artifact.Entrypoint != "" || artifact.UnpackedSize != 0 ||
			artifact.FileCount != 0 || len(artifact.Hooks) != 0 {
			return fmt.Errorf("self-replace requires raw format without entrypoint, size metadata, or hooks")
		}
	case ArtifactBundle:
		if artifact.Strategy != InstallStrategyBundle ||
			(artifact.Format != ArchiveTarGz && artifact.Format != ArchiveZIP) {
			return fmt.Errorf("bundle format must be tar.gz or zip")
		}
		if len(artifact.Entrypoint) > 1024 {
			return fmt.Errorf("bundle entrypoint exceeds 1024 bytes")
		}
		if _, err := safeRelativePath(artifact.Entrypoint); err != nil {
			return fmt.Errorf("invalid bundle entrypoint: %w", err)
		}
		if artifact.UnpackedSize < 0 || artifact.FileCount < 0 {
			return fmt.Errorf("bundle unpackedSize and fileCount must not be negative")
		}
	default:
		return fmt.Errorf("kind must be binary or bundle")
	}
	if len(artifact.Hooks) > 3 {
		return fmt.Errorf("too many hooks")
	}
	phases := make(map[HookPhase]struct{}, len(artifact.Hooks))
	for index := range artifact.Hooks {
		if err := validateHook(artifact.Hooks[index]); err != nil {
			return fmt.Errorf("hook %d: %w", index, err)
		}
		if _, duplicate := phases[artifact.Hooks[index].Phase]; duplicate {
			return fmt.Errorf("duplicate hook phase %q", artifact.Hooks[index].Phase)
		}
		phases[artifact.Hooks[index].Phase] = struct{}{}
	}
	return nil
}

func validateHook(hook Hook) error {
	if hook.Phase != HookPreflight && hook.Phase != HookPostInstall && hook.Phase != HookVerify {
		return fmt.Errorf("unsupported phase %q", hook.Phase)
	}
	if len(hook.Path) > 1024 {
		return fmt.Errorf("path exceeds 1024 bytes")
	}
	if _, err := safeRelativePath(hook.Path); err != nil {
		return fmt.Errorf("invalid path: %w", err)
	}
	if len(hook.Args) > 16 {
		return fmt.Errorf("too many arguments")
	}
	for _, argument := range hook.Args {
		if argument == "" || len(argument) > 1024 || strings.ContainsRune(argument, '\x00') {
			return fmt.Errorf("argument must contain 1 to 1024 bytes without NUL")
		}
	}
	if hook.TimeoutSeconds < 1 || hook.TimeoutSeconds > 300 {
		return fmt.Errorf("timeoutSeconds must be between 1 and 300")
	}
	return nil
}

func validateInstallMode(encoded string) error {
	if len(encoded) != 4 || encoded[0] != '0' {
		return fmt.Errorf("install mode must be a four-digit octal string")
	}
	for index := 1; index < len(encoded); index++ {
		if encoded[index] < '0' || encoded[index] > '7' {
			return fmt.Errorf("install mode must be a four-digit octal string")
		}
	}
	mode, err := strconv.ParseUint(encoded, 8, 32)
	if err != nil || mode > 0o777 || mode&0o111 == 0 {
		return fmt.Errorf("install mode must include an executable bit")
	}
	return nil
}

func cloneManifest(source Manifest) Manifest {
	clone := source
	if source.Product != nil {
		product := *source.Product
		clone.Product = &product
	}
	clone.Artifacts = make([]Artifact, len(source.Artifacts))
	for index := range source.Artifacts {
		clone.Artifacts[index] = cloneArtifact(source.Artifacts[index])
	}
	return clone
}

func cloneArtifact(source Artifact) Artifact {
	clone := source
	clone.Hooks = make([]Hook, len(source.Hooks))
	for index := range source.Hooks {
		clone.Hooks[index] = source.Hooks[index]
		clone.Hooks[index].Args = append([]string(nil), source.Hooks[index].Args...)
	}
	return clone
}
