package artifact

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

const (
	maxPathBytes    = 1024
	maxSegmentBytes = 255
)

var ErrInvalidPath = errors.New("invalid artifact path")

func NormalizePath(rawEscapedPath string) (string, error) {
	decoded, err := url.PathUnescape(rawEscapedPath)
	if err != nil {
		return "", fmt.Errorf("%w: malformed percent encoding", ErrInvalidPath)
	}
	if decoded != rawEscapedPath || strings.Contains(decoded, "%") {
		return "", fmt.Errorf("%w: path is not canonically encoded", ErrInvalidPath)
	}
	if len(decoded) == 0 || len(decoded) > maxPathBytes {
		return "", fmt.Errorf("%w: path length is outside 1..%d bytes", ErrInvalidPath, maxPathBytes)
	}
	for _, segment := range strings.Split(decoded, "/") {
		if len(segment) == 0 || len(segment) > maxSegmentBytes || segment == "." || segment == ".." {
			return "", fmt.Errorf("%w: invalid path segment", ErrInvalidPath)
		}
		for index := 0; index < len(segment); index++ {
			if !isUnreserved(segment[index]) {
				return "", fmt.Errorf("%w: path contains a reserved character", ErrInvalidPath)
			}
		}
	}
	return decoded, nil
}

func isUnreserved(value byte) bool {
	return value >= 'a' && value <= 'z' ||
		value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' ||
		value == '-' || value == '.' || value == '_' || value == '~'
}
