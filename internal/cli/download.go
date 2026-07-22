package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type expectedArtifact struct {
	SHA256 string
	Size   int64
}

func (a application) saveDownload(source io.Reader, destination string, force bool, expected expectedArtifact) error {
	if destination == "-" {
		if force {
			return usageError{message: "--force cannot be used when output is stdout"}
		}
		if err := copyAndVerify(a.stdout, source, expected); err != nil {
			return fmt.Errorf("verify streamed download after writing stdout: %w", err)
		}
		return nil
	}
	destination = filepath.Clean(destination)
	if !force {
		if _, err := os.Lstat(destination); err == nil {
			return fmt.Errorf("output file %q already exists; use --force to replace it", destination)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect output path: %w", err)
		}
	}
	directory := filepath.Dir(destination)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".artifactctl-download-*")
	if err != nil {
		return fmt.Errorf("create temporary download file: %w", err)
	}
	temporaryPath := temporary.Name()
	keep := false
	defer func() {
		_ = temporary.Close()
		if !keep {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := copyAndVerify(temporary, source, expected); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync downloaded file: %w", err)
	}
	if err := temporary.Chmod(0o644); err != nil {
		return fmt.Errorf("set downloaded file permissions: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close downloaded file: %w", err)
	}
	if force {
		if err := os.Rename(temporaryPath, destination); err != nil {
			return fmt.Errorf("replace output file: %w", err)
		}
	} else {
		if err := os.Link(temporaryPath, destination); err != nil {
			if _, statErr := os.Lstat(destination); statErr == nil {
				return fmt.Errorf("output file %q already exists; use --force to replace it", destination)
			}
			return fmt.Errorf("install downloaded file: %w", err)
		}
		if err := os.Remove(temporaryPath); err != nil {
			return fmt.Errorf("remove temporary download link: %w", err)
		}
	}
	keep = true
	_, _ = fmt.Fprintf(a.stderr, "downloaded %s (%d bytes, sha256:%s)\n", destination, expected.Size, expected.SHA256)
	return nil
}

func copyAndVerify(destination io.Writer, source io.Reader, expected expectedArtifact) error {
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(destination, hash), source)
	if err != nil {
		return fmt.Errorf("write downloaded artifact: %w", err)
	}
	if written != expected.Size {
		return fmt.Errorf("downloaded size mismatch: got %d, want %d", written, expected.Size)
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if actual != expected.SHA256 {
		return fmt.Errorf("downloaded SHA-256 mismatch: got %s, want %s", actual, expected.SHA256)
	}
	return nil
}
