package forgeupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
)

func TestCopyVerifiedEnforcesSizeLimitAndDigest(t *testing.T) {
	content := []byte("verified bytes")
	artifact := artifactForBytes(content)
	var output bytes.Buffer
	if err := CopyVerified(t.Context(), &output, bytes.NewReader(content), artifact, DownloadLimits{MaxBytes: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), content) {
		t.Fatalf("output = %q", output.Bytes())
	}

	output.Reset()
	if err := CopyVerified(t.Context(), &output, bytes.NewReader(append(content, 'x')), artifact, DownloadLimits{MaxBytes: 1 << 20}); !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("extra-byte error = %v", err)
	}
	if !bytes.Equal(output.Bytes(), content) {
		t.Fatalf("extra byte was written: %q", output.Bytes())
	}

	output.Reset()
	corrupt := artifact
	corrupt.SHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := CopyVerified(t.Context(), &output, bytes.NewReader(content), corrupt, DownloadLimits{MaxBytes: 1 << 20}); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("checksum error = %v", err)
	}

	if err := CopyVerified(t.Context(), &output, bytes.NewReader(content), artifact, DownloadLimits{MaxBytes: int64(len(content) - 1)}); !errors.Is(err, ErrDownloadTooLarge) {
		t.Fatalf("limit error = %v", err)
	}
}

func TestCopyVerifiedHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var output bytes.Buffer
	content := []byte("bytes")
	if err := CopyVerified(ctx, &output, bytes.NewReader(content), artifactForBytes(content), DownloadLimits{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func artifactForBytes(content []byte) Artifact {
	digest := sha256.Sum256(content)
	return Artifact{
		SHA256: hex.EncodeToString(digest[:]),
		Size:   int64(len(content)),
	}
}
