package forgeupdate

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
)

const DefaultMaxDownloadBytes int64 = 8 << 30

type DownloadLimits struct {
	MaxBytes int64
}

// CopyVerified copies exactly artifact.Size bytes, rejects a trailing byte,
// enforces a finite maximum, and verifies SHA-256 before returning success.
func CopyVerified(ctx context.Context, destination io.Writer, source io.Reader, artifact Artifact, limits DownloadLimits) error {
	maximum := limits.MaxBytes
	if maximum <= 0 {
		maximum = DefaultMaxDownloadBytes
	}
	if artifact.Size < 0 {
		return fmt.Errorf("%w: negative expected size", ErrSizeMismatch)
	}
	if artifact.Size > maximum {
		return fmt.Errorf("%w: expected %d bytes, limit %d", ErrDownloadTooLarge, artifact.Size, maximum)
	}
	expected, err := hex.DecodeString(artifact.SHA256)
	if err != nil || len(expected) != sha256.Size {
		return fmt.Errorf("%w: invalid expected SHA-256", ErrChecksumMismatch)
	}
	reader := &contextReader{ctx: ctx, reader: source}
	limited := &io.LimitedReader{R: reader, N: artifact.Size}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(destination, hash), limited)
	if err != nil {
		return fmt.Errorf("forgeupdate: copy download: %w", err)
	}
	if written != artifact.Size {
		return fmt.Errorf("%w: got %d, want %d", ErrSizeMismatch, written, artifact.Size)
	}
	var extra [1]byte
	count, readErr := reader.Read(extra[:])
	if count != 0 {
		return fmt.Errorf("%w: response contains more than %d bytes", ErrSizeMismatch, artifact.Size)
	}
	if readErr == nil {
		return fmt.Errorf("forgeupdate: finish download: %w", io.ErrNoProgress)
	}
	if readErr != nil && readErr != io.EOF {
		return fmt.Errorf("forgeupdate: finish download: %w", readErr)
	}
	if actual := hash.Sum(nil); subtle.ConstantTimeCompare(actual, expected) != 1 {
		return fmt.Errorf("%w: got %s, want %s", ErrChecksumMismatch, hex.EncodeToString(actual), artifact.SHA256)
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *contextReader) Read(destination []byte) (int, error) {
	select {
	case <-reader.ctx.Done():
		return 0, reader.ctx.Err()
	default:
		return reader.reader.Read(destination)
	}
}
