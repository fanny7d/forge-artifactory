package forgeupdate

import (
	"fmt"
	"strings"
)

type semanticVersion struct {
	major      string
	minor      string
	patch      string
	prerelease []string
}

// CompareVersions compares two strict SemVer 2.0.0 versions. It returns -1,
// 0, or 1. Build metadata is validated but does not affect precedence.
func CompareVersions(left, right string) (int, error) {
	a, err := parseSemanticVersion(left)
	if err != nil {
		return 0, fmt.Errorf("parse left version: %w", err)
	}
	b, err := parseSemanticVersion(right)
	if err != nil {
		return 0, fmt.Errorf("parse right version: %w", err)
	}
	for _, pair := range [][2]string{
		{a.major, b.major},
		{a.minor, b.minor},
		{a.patch, b.patch},
	} {
		if comparison := compareNumericIdentifier(pair[0], pair[1]); comparison != 0 {
			return comparison, nil
		}
	}
	return comparePrerelease(a.prerelease, b.prerelease), nil
}

func parseSemanticVersion(raw string) (semanticVersion, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return semanticVersion{}, fmt.Errorf("version must not be empty or contain surrounding whitespace")
	}
	coreAndPrerelease := raw
	if index := strings.IndexByte(raw, '+'); index >= 0 {
		coreAndPrerelease = raw[:index]
		if err := validateIdentifiers(raw[index+1:], false); err != nil {
			return semanticVersion{}, fmt.Errorf("invalid build metadata: %w", err)
		}
	}
	core := coreAndPrerelease
	var prerelease []string
	if index := strings.IndexByte(coreAndPrerelease, '-'); index >= 0 {
		core = coreAndPrerelease[:index]
		encoded := coreAndPrerelease[index+1:]
		if err := validateIdentifiers(encoded, true); err != nil {
			return semanticVersion{}, fmt.Errorf("invalid prerelease: %w", err)
		}
		prerelease = strings.Split(encoded, ".")
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return semanticVersion{}, fmt.Errorf("version core must contain major, minor, and patch")
	}
	for _, part := range parts {
		if !isNumericIdentifier(part) || (len(part) > 1 && part[0] == '0') {
			return semanticVersion{}, fmt.Errorf("invalid numeric identifier %q", part)
		}
	}
	return semanticVersion{
		major: parts[0], minor: parts[1], patch: parts[2], prerelease: prerelease,
	}, nil
}

func validateIdentifiers(raw string, forbidNumericLeadingZero bool) error {
	if raw == "" {
		return fmt.Errorf("identifier list must not be empty")
	}
	for _, identifier := range strings.Split(raw, ".") {
		if identifier == "" {
			return fmt.Errorf("identifier must not be empty")
		}
		for index := 0; index < len(identifier); index++ {
			character := identifier[index]
			if !((character >= '0' && character <= '9') ||
				(character >= 'A' && character <= 'Z') ||
				(character >= 'a' && character <= 'z') ||
				character == '-') {
				return fmt.Errorf("identifier %q contains an invalid character", identifier)
			}
		}
		if forbidNumericLeadingZero && isNumericIdentifier(identifier) &&
			len(identifier) > 1 && identifier[0] == '0' {
			return fmt.Errorf("numeric identifier %q has a leading zero", identifier)
		}
	}
	return nil
}

func isNumericIdentifier(raw string) bool {
	if raw == "" {
		return false
	}
	for index := 0; index < len(raw); index++ {
		if raw[index] < '0' || raw[index] > '9' {
			return false
		}
	}
	return true
}

func compareNumericIdentifier(left, right string) int {
	if len(left) < len(right) {
		return -1
	}
	if len(left) > len(right) {
		return 1
	}
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}

func comparePrerelease(left, right []string) int {
	if len(left) == 0 && len(right) == 0 {
		return 0
	}
	if len(left) == 0 {
		return 1
	}
	if len(right) == 0 {
		return -1
	}
	limit := len(left)
	if len(right) < limit {
		limit = len(right)
	}
	for index := 0; index < limit; index++ {
		a := left[index]
		b := right[index]
		aNumeric := isNumericIdentifier(a)
		bNumeric := isNumericIdentifier(b)
		switch {
		case aNumeric && bNumeric:
			if comparison := compareNumericIdentifier(a, b); comparison != 0 {
				return comparison
			}
		case aNumeric:
			return -1
		case bNumeric:
			return 1
		case a < b:
			return -1
		case a > b:
			return 1
		}
	}
	if len(left) < len(right) {
		return -1
	}
	if len(left) > len(right) {
		return 1
	}
	return 0
}
