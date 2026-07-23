package release

import "strings"

func ValidVersion(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	if strings.Count(value, "+") > 1 {
		return false
	}
	main, build, hasBuild := strings.Cut(value, "+")
	if hasBuild && !validSemverIdentifiers(build, false) {
		return false
	}
	coreVersion, prerelease, hasPrerelease := strings.Cut(main, "-")
	if hasPrerelease && !validSemverIdentifiers(prerelease, true) {
		return false
	}
	core := strings.Split(coreVersion, ".")
	if len(core) != 3 {
		return false
	}
	for _, identifier := range core {
		if !validNumericIdentifier(identifier, true) {
			return false
		}
	}
	return true
}

func validSemverIdentifiers(value string, rejectNumericLeadingZero bool) bool {
	identifiers := strings.Split(value, ".")
	for _, identifier := range identifiers {
		if identifier == "" {
			return false
		}
		numeric := true
		for _, character := range identifier {
			if character < '0' || character > '9' {
				numeric = false
			}
			if !((character >= '0' && character <= '9') ||
				(character >= 'A' && character <= 'Z') ||
				(character >= 'a' && character <= 'z') ||
				character == '-') {
				return false
			}
		}
		if rejectNumericLeadingZero && numeric && len(identifier) > 1 && identifier[0] == '0' {
			return false
		}
	}
	return true
}

func validNumericIdentifier(value string, rejectLeadingZero bool) bool {
	if value == "" || (rejectLeadingZero && len(value) > 1 && value[0] == '0') {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}
