package artifact

import (
	"strings"
	"testing"
)

func TestNormalizePathAcceptsCanonicalUnreservedSegments(t *testing.T) {
	for _, path := range []string{
		"edgecli",
		"linux/arm64/edgecli-1.2.3.tar.gz",
		"symbols/debug_file~1",
	} {
		t.Run(path, func(t *testing.T) {
			got, err := NormalizePath(path)
			if err != nil {
				t.Fatalf("NormalizePath(%q) error = %v", path, err)
			}
			if got != path {
				t.Fatalf("NormalizePath(%q) = %q", path, got)
			}
		})
	}
}

func TestNormalizePathRejectsAmbiguousAndEncodedSeparators(t *testing.T) {
	for _, path := range []string{
		"../x",
		"./x",
		"a//b",
		"/a",
		"a/",
		`a\b`,
		"a/%2fb",
		"a/%2Fb",
		"a/%5cb",
		"a/%252fb",
		"a/%41",
		"a/%zz",
		"a/white space",
		"a/控制",
		"a/line\nbreak",
	} {
		t.Run(path, func(t *testing.T) {
			if _, err := NormalizePath(path); err == nil {
				t.Fatalf("NormalizePath(%q) succeeded", path)
			}
		})
	}
}

func TestNormalizePathEnforcesDecodedByteLimits(t *testing.T) {
	for _, path := range []string{
		strings.Repeat("a", 256),
		strings.Repeat("a", 255) + "/" + strings.Repeat("b", 255) + "/" + strings.Repeat("c", 255) + "/" + strings.Repeat("d", 255) + "/x",
	} {
		if _, err := NormalizePath(path); err == nil {
			t.Fatalf("NormalizePath(path length %d) succeeded", len(path))
		}
	}
}
