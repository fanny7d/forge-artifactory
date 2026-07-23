package cli

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	api "superfan.myasustor.com/fanchao/artifact-repository/internal/api"
	releasedomain "superfan.myasustor.com/fanchao/artifact-repository/internal/release"
)

type signedManifest struct {
	SchemaVersion int                      `json:"schemaVersion"`
	Repository    string                   `json:"repository"`
	Package       string                   `json:"package"`
	Product       *signedManifestProduct   `json:"product,omitempty"`
	Version       string                   `json:"version"`
	PublishedAt   time.Time                `json:"publishedAt"`
	Artifacts     []signedManifestArtifact `json:"artifacts"`
}

type signedManifestProduct struct {
	Slug    string `json:"slug"`
	Command string `json:"command"`
}

type signedManifestArtifact struct {
	Path      string                     `json:"path"`
	Filename  string                     `json:"filename"`
	OS        string                     `json:"os"`
	Arch      string                     `json:"arch"`
	Variant   string                     `json:"variant"`
	Role      string                     `json:"role"`
	MediaType string                     `json:"mediaType"`
	SHA256    string                     `json:"sha256"`
	Size      int64                      `json:"size"`
	Install   *releasedomain.InstallSpec `json:"install,omitempty"`
}

func verifyResolution(publicKeyPath string, ref packageReference, selection channelSelection, resolved api.ResolveResponse) error {
	publicKey, keyID, err := loadPublicKey(publicKeyPath)
	if err != nil {
		return err
	}
	if resolved.KeyId != keyID {
		return fmt.Errorf("response key ID %q does not match trusted key %q", resolved.KeyId, keyID)
	}
	manifestBytes, err := decodeBase64URL(resolved.Manifest)
	if err != nil {
		return fmt.Errorf("decode manifest: %w", err)
	}
	signature, err := decodeBase64URL(resolved.Signature)
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}
	if !ed25519.Verify(publicKey, manifestBytes, signature) {
		return fmt.Errorf("Ed25519 signature is invalid")
	}
	var manifest signedManifest
	decoder := json.NewDecoder(bytes.NewReader(manifestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return fmt.Errorf("decode signed manifest: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return fmt.Errorf("signed manifest must contain exactly one JSON object")
	}
	if (manifest.SchemaVersion != 1 && manifest.SchemaVersion != 2) ||
		manifest.Repository != ref.Repository || manifest.Package != ref.Package || manifest.Version != resolved.Version {
		return fmt.Errorf("signed manifest identity does not match the resolved package")
	}
	if manifest.SchemaVersion == 1 && manifest.Product != nil {
		return fmt.Errorf("schema v1 manifest unexpectedly contains product metadata")
	}
	if manifest.SchemaVersion == 2 {
		if manifest.Product == nil || manifest.Product.Slug != ref.Package || manifest.Product.Command == "" {
			return fmt.Errorf("schema v2 manifest product identity does not match the resolved package")
		}
	}
	for _, artifact := range manifest.Artifacts {
		if artifact.OS == selection.OS && artifact.Arch == selection.Arch && artifact.Variant == selection.Variant && artifact.Role == selection.Role {
			if artifact.Path != resolved.Artifact.Path || artifact.SHA256 != resolved.Artifact.Sha256 || artifact.Size != resolved.Artifact.Size ||
				resolved.Artifact.Os != selection.OS || resolved.Artifact.Arch != selection.Arch || resolved.Artifact.Variant != selection.Variant || resolved.Artifact.Role != selection.Role {
				return fmt.Errorf("resolved artifact metadata does not match the signed manifest")
			}
			if manifest.SchemaVersion == 2 {
				if artifact.Install == nil {
					return fmt.Errorf("schema v2 selected artifact has no install plan")
				}
				if err := artifact.Install.Validate(); err != nil {
					return fmt.Errorf("schema v2 selected artifact install plan is invalid: %w", err)
				}
			}
			return nil
		}
	}
	return fmt.Errorf("selected artifact is not present in the signed manifest")
}

func loadPublicKey(path string) (ed25519.PublicKey, string, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read trusted public key: %w", err)
	}
	block, trailing := pem.Decode(encoded)
	if block == nil || len(bytes.TrimSpace(trailing)) != 0 {
		return nil, "", fmt.Errorf("trusted public key must be exactly one PEM block")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, "", fmt.Errorf("parse trusted public key: %w", err)
	}
	publicKey, ok := parsed.(ed25519.PublicKey)
	if !ok || len(publicKey) != ed25519.PublicKeySize {
		return nil, "", fmt.Errorf("trusted public key is not Ed25519")
	}
	fingerprint := sha256.Sum256(publicKey)
	return publicKey, "ed25519:" + hex.EncodeToString(fingerprint[:]), nil
}
