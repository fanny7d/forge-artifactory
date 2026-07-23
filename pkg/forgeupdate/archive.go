package forgeupdate

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	DefaultMaxUnpackedBytes int64 = 16 << 30
	DefaultMaxArchiveFiles        = 100_000
)

type extractionLimits struct {
	maxBytes int64
	maxFiles int
}

type extractionState struct {
	root       string
	seen       map[string]struct{}
	totalBytes int64
	fileCount  int
	limits     extractionLimits
}

func safeRelativePath(raw string) (string, error) {
	if raw == "" || strings.Contains(raw, "\\") || strings.ContainsRune(raw, '\x00') ||
		strings.HasPrefix(raw, "/") {
		return "", fmt.Errorf("%w: path %q is not a portable relative path", ErrUnsafeArchive, raw)
	}
	cleaned := path.Clean(raw)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || cleaned != raw {
		return "", fmt.Errorf("%w: path %q is not canonical", ErrUnsafeArchive, raw)
	}
	converted := filepath.FromSlash(cleaned)
	if filepath.IsAbs(converted) || filepath.VolumeName(converted) != "" {
		return "", fmt.Errorf("%w: path %q is absolute", ErrUnsafeArchive, raw)
	}
	return converted, nil
}

func newExtractionState(root string, limits extractionLimits) *extractionState {
	return &extractionState{
		root: root, seen: make(map[string]struct{}), limits: limits,
	}
}

func (state *extractionState) reserve(raw string, directory bool, size int64) (string, error) {
	trimmed := raw
	if directory {
		trimmed = strings.TrimSuffix(trimmed, "/")
	}
	relative, err := safeRelativePath(trimmed)
	if err != nil {
		return "", err
	}
	// Reject case-only duplicates on every platform. This avoids archives that
	// are safe on a case-sensitive builder but collide on common macOS/Windows
	// filesystems.
	key := strings.ToLower(filepath.ToSlash(relative))
	if _, duplicate := state.seen[key]; duplicate {
		return "", fmt.Errorf("%w: duplicate path %q", ErrUnsafeArchive, raw)
	}
	state.seen[key] = struct{}{}
	if directory {
		return relative, nil
	}
	if size < 0 {
		return "", fmt.Errorf("%w: negative size for %q", ErrUnsafeArchive, raw)
	}
	if state.fileCount >= state.limits.maxFiles {
		return "", fmt.Errorf("%w: file count exceeds %d", ErrUnsafeArchive, state.limits.maxFiles)
	}
	if size > state.limits.maxBytes-state.totalBytes {
		return "", fmt.Errorf("%w: unpacked size exceeds %d", ErrUnsafeArchive, state.limits.maxBytes)
	}
	state.fileCount++
	state.totalBytes += size
	return relative, nil
}

func (state *extractionState) createDirectory(relative string) error {
	target := filepath.Join(state.root, relative)
	if err := ensureWithinRoot(state.root, target); err != nil {
		return err
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("forgeupdate: create archive directory: %w", err)
	}
	return nil
}

func (state *extractionState) createFile(relative string, mode os.FileMode, size int64, source io.Reader) error {
	target := filepath.Join(state.root, relative)
	if err := ensureWithinRoot(state.root, target); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("forgeupdate: create archive parent: %w", err)
	}
	fileMode := os.FileMode(0o644)
	if mode.Perm()&0o111 != 0 {
		fileMode = 0o755
	}
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return fmt.Errorf("%w: create archive file %q: %v", ErrUnsafeArchive, relative, err)
	}
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = os.Remove(target)
		}
	}()
	written, err := io.CopyN(file, source, size)
	if err != nil {
		return fmt.Errorf("%w: extract %q: %v", ErrUnsafeArchive, relative, err)
	}
	if written != size {
		return fmt.Errorf("%w: extracted size mismatch for %q", ErrUnsafeArchive, relative)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("forgeupdate: sync extracted file %q: %w", relative, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("forgeupdate: close extracted file %q: %w", relative, err)
	}
	keep = true
	return nil
}

func ensureWithinRoot(root, target string) error {
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return fmt.Errorf("%w: extracted path escapes staging root", ErrUnsafeArchive)
	}
	return nil
}

func extractTarGz(archive *os.File, state *extractionState) error {
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("forgeupdate: seek tar.gz: %w", err)
	}
	compressed, err := gzip.NewReader(archive)
	if err != nil {
		return fmt.Errorf("%w: open gzip stream: %v", ErrUnsafeArchive, err)
	}
	defer func() { _ = compressed.Close() }()
	reader := tar.NewReader(compressed)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: read tar header: %v", ErrUnsafeArchive, err)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			relative, reserveErr := state.reserve(header.Name, true, 0)
			if reserveErr != nil {
				return reserveErr
			}
			if err := state.createDirectory(relative); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			relative, reserveErr := state.reserve(header.Name, false, header.Size)
			if reserveErr != nil {
				return reserveErr
			}
			if err := state.createFile(relative, header.FileInfo().Mode(), header.Size, reader); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: tar entry %q has unsupported type %d", ErrUnsafeArchive, header.Name, header.Typeflag)
		}
	}
	if err := compressed.Close(); err != nil {
		return fmt.Errorf("%w: finish gzip stream: %v", ErrUnsafeArchive, err)
	}
	return nil
}

func extractZIP(archive *os.File, state *extractionState) error {
	info, err := archive.Stat()
	if err != nil {
		return fmt.Errorf("forgeupdate: stat zip archive: %w", err)
	}
	reader, err := zip.NewReader(archive, info.Size())
	if err != nil {
		return fmt.Errorf("%w: open zip: %v", ErrUnsafeArchive, err)
	}
	for _, entry := range reader.File {
		mode := entry.Mode()
		directory := entry.FileInfo().IsDir()
		if !directory && !mode.IsRegular() {
			return fmt.Errorf("%w: zip entry %q is a symlink or special file", ErrUnsafeArchive, entry.Name)
		}
		if entry.UncompressedSize64 > uint64(^uint64(0)>>1) {
			return fmt.Errorf("%w: zip entry %q is too large", ErrUnsafeArchive, entry.Name)
		}
		size := int64(entry.UncompressedSize64)
		relative, err := state.reserve(entry.Name, directory, size)
		if err != nil {
			return err
		}
		if directory {
			if err := state.createDirectory(relative); err != nil {
				return err
			}
			continue
		}
		source, err := entry.Open()
		if err != nil {
			return fmt.Errorf("%w: open zip entry %q: %v", ErrUnsafeArchive, entry.Name, err)
		}
		extractErr := state.createFile(relative, mode, size, source)
		var trailing [1]byte
		trailingCount, trailingErr := io.ReadFull(source, trailing[:])
		closeErr := source.Close()
		if extractErr != nil {
			return extractErr
		}
		if trailingCount != 0 {
			return fmt.Errorf("%w: zip entry %q exceeds its declared size", ErrUnsafeArchive, entry.Name)
		}
		if trailingErr != nil && trailingErr != io.EOF {
			return fmt.Errorf("%w: finish zip entry %q: %v", ErrUnsafeArchive, entry.Name, trailingErr)
		}
		if closeErr != nil {
			return fmt.Errorf("%w: finish zip entry %q: %v", ErrUnsafeArchive, entry.Name, closeErr)
		}
	}
	return nil
}

func validateExtractedEntrypoint(root, relative string, mode os.FileMode) (string, error) {
	converted, err := safeRelativePath(relative)
	if err != nil {
		return "", err
	}
	target := filepath.Join(root, converted)
	if err := ensureWithinRoot(root, target); err != nil {
		return "", err
	}
	info, err := os.Lstat(target)
	if err != nil {
		return "", fmt.Errorf("%w: entrypoint is missing: %v", ErrUnsafeArchive, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: entrypoint is not a regular file", ErrUnsafeArchive)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(target, mode.Perm()); err != nil {
			return "", fmt.Errorf("forgeupdate: set signed entrypoint mode: %w", err)
		}
		file, err := os.OpenFile(target, os.O_RDWR, 0)
		if err != nil {
			return "", fmt.Errorf("forgeupdate: reopen executable entrypoint: %w", err)
		}
		syncErr := file.Sync()
		closeErr := file.Close()
		if syncErr != nil {
			return "", fmt.Errorf("forgeupdate: sync executable entrypoint: %w", syncErr)
		}
		if closeErr != nil {
			return "", fmt.Errorf("forgeupdate: close executable entrypoint: %w", closeErr)
		}
	}
	return target, nil
}
