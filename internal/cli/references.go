package cli

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	artifactdomain "superfan.myasustor.com/fanchao/artifact-repository/internal/artifact"
)

var (
	validName               = regexp.MustCompile(`^[a-z][a-z0-9._-]{1,63}$`)
	validCoordinate         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
	validOptionalCoordinate = regexp.MustCompile(`^[A-Za-z0-9._+-]{0,64}$`)
)

type artifactReference struct {
	Repository string
	Path       string
}

type packageReference struct {
	Repository string
	Package    string
}

type channelSelection struct {
	Channel  string
	OS       string
	Arch     string
	Variant  string
	Role     string
	Redirect bool
}

func parseArtifactReference(raw string) (artifactReference, error) {
	repository, artifactPath, ok := strings.Cut(strings.TrimSpace(raw), "/")
	if !ok || !validName.MatchString(repository) {
		return artifactReference{}, fmt.Errorf("artifact reference must be <repository/artifact-path>")
	}
	normalized, err := artifactdomain.NormalizePath(artifactPath)
	if err != nil {
		return artifactReference{}, fmt.Errorf("invalid artifact path: %w", err)
	}
	return artifactReference{Repository: repository, Path: normalized}, nil
}

func parsePackageReference(raw string) (packageReference, error) {
	repository, packageName, ok := strings.Cut(strings.TrimSpace(raw), "/")
	if !ok || !validName.MatchString(repository) || !validName.MatchString(packageName) {
		return packageReference{}, fmt.Errorf("package reference must be <repository/package>")
	}
	return packageReference{Repository: repository, Package: packageName}, nil
}

func (selection channelSelection) validate() error {
	if selection.Channel != "candidate" && selection.Channel != "stable" {
		return fmt.Errorf("channel must be candidate or stable")
	}
	if !validCoordinate.MatchString(selection.OS) || !validCoordinate.MatchString(selection.Arch) {
		return fmt.Errorf("os and arch must be valid coordinates")
	}
	for name, value := range map[string]string{"variant": selection.Variant, "role": selection.Role} {
		if !validOptionalCoordinate.MatchString(value) {
			return fmt.Errorf("%s must be empty or a valid coordinate", name)
		}
	}
	return nil
}

func encodeProperties(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if len(raw) > 16<<10 {
		return "", fmt.Errorf("properties JSON exceeds 16 KiB")
	}
	var object map[string]any
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&object); err != nil || object == nil {
		return "", fmt.Errorf("properties must be a JSON object")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("properties must contain exactly one JSON object")
	}
	return base64.RawURLEncoding.EncodeToString([]byte(raw)), nil
}
