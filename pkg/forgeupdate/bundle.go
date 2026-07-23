package forgeupdate

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

type BundleOptions struct {
	Root             string
	MaxDownloadBytes int64
	MaxUnpackedBytes int64
	MaxFiles         int
	Executor         HookExecutor
}

type BundleReceipt struct {
	versionPath      string
	entrypoint       string
	activeEntrypoint string
	pointer          bundlePointer
	state            *bundleReceiptState
}

type bundleReceiptState struct {
	mu              sync.Mutex
	active          bool
	pointerRestored bool
}

func (receipt *BundleReceipt) VersionPath() string {
	if receipt == nil {
		return ""
	}
	return receipt.versionPath
}

func (receipt *BundleReceipt) Entrypoint() string {
	if receipt == nil {
		return ""
	}
	return receipt.entrypoint
}

// ActiveEntrypoint is the stable path through Root/current. It continues to
// identify the active bundle across later atomic version switches.
func (receipt *BundleReceipt) ActiveEntrypoint() string {
	if receipt == nil {
		return ""
	}
	return receipt.activeEntrypoint
}

// InstallBundle verifies an archive, safely extracts it into a private staging
// directory, validates its entrypoint, commits it as Root/versions/<version>,
// and atomically switches Root/current to the new version. Windows requires an
// out-of-process helper and returns ErrHelperRequired before writing anything.
func InstallBundle(ctx context.Context, source io.Reader, release VerifiedRelease, options BundleOptions) (*BundleReceipt, error) {
	if err := ensureBundlePlatform(); err != nil {
		return nil, err
	}
	artifact := release.artifact
	manifest := release.manifest
	if artifact.Kind != ArtifactBundle {
		return nil, fmt.Errorf("forgeupdate: bundle installer requires a bundle artifact")
	}
	if _, err := parseSemanticVersion(manifest.Version); err != nil {
		return nil, fmt.Errorf("forgeupdate: invalid release version: %w", err)
	}
	if options.Root == "" {
		return nil, fmt.Errorf("forgeupdate: bundle root is required")
	}
	root, err := filepath.Abs(options.Root)
	if err != nil {
		return nil, fmt.Errorf("forgeupdate: resolve bundle root: %w", err)
	}
	versions := filepath.Join(root, "versions")
	if err := os.MkdirAll(versions, 0o755); err != nil {
		return nil, fmt.Errorf("forgeupdate: create versions directory: %w", err)
	}
	versionsInfo, err := os.Lstat(versions)
	if err != nil || !versionsInfo.IsDir() || versionsInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("forgeupdate: versions path must be a real directory")
	}
	versionPath := filepath.Join(versions, manifest.Version)
	if _, err := os.Lstat(versionPath); err == nil {
		return nil, fmt.Errorf("%w: %s", ErrAlreadyInstalled, manifest.Version)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("forgeupdate: inspect version path: %w", err)
	}
	archive, err := os.CreateTemp(versions, ".forgeupdate-archive-*")
	if err != nil {
		return nil, fmt.Errorf("forgeupdate: create archive staging file: %w", err)
	}
	archivePath := archive.Name()
	defer func() {
		_ = archive.Close()
		_ = os.Remove(archivePath)
	}()
	if err := CopyVerified(ctx, archive, source, artifact, DownloadLimits{MaxBytes: options.MaxDownloadBytes}); err != nil {
		return nil, err
	}
	if err := archive.Sync(); err != nil {
		return nil, fmt.Errorf("forgeupdate: sync bundle archive: %w", err)
	}
	staging, err := os.MkdirTemp(versions, "."+manifest.Version+".staging-*")
	if err != nil {
		return nil, fmt.Errorf("forgeupdate: create bundle staging directory: %w", err)
	}
	stagingPresent := true
	defer func() {
		if stagingPresent {
			_ = os.RemoveAll(staging)
		}
	}()
	maximumBytes := options.MaxUnpackedBytes
	if maximumBytes <= 0 {
		maximumBytes = DefaultMaxUnpackedBytes
	}
	maximumFiles := options.MaxFiles
	if maximumFiles <= 0 {
		maximumFiles = DefaultMaxArchiveFiles
	}
	if artifact.UnpackedSize > maximumBytes {
		return nil, fmt.Errorf("%w: declared unpacked size %d exceeds %d", ErrUnsafeArchive, artifact.UnpackedSize, maximumBytes)
	}
	if artifact.FileCount > maximumFiles {
		return nil, fmt.Errorf("%w: declared file count %d exceeds %d", ErrUnsafeArchive, artifact.FileCount, maximumFiles)
	}
	state := newExtractionState(staging, extractionLimits{maxBytes: maximumBytes, maxFiles: maximumFiles})
	switch artifact.Format {
	case ArchiveTarGz:
		err = extractTarGz(archive, state)
	case ArchiveZIP:
		err = extractZIP(archive, state)
	default:
		err = fmt.Errorf("%w: unsupported bundle format %q", ErrUnsafeArchive, artifact.Format)
	}
	if err != nil {
		return nil, err
	}
	if (artifact.UnpackedSize > 0 && state.totalBytes != artifact.UnpackedSize) ||
		(artifact.FileCount > 0 && state.fileCount != artifact.FileCount) {
		return nil, fmt.Errorf("%w: extracted content is %d bytes/%d files, want %d bytes/%d files",
			ErrUnsafeArchive, state.totalBytes, state.fileCount, artifact.UnpackedSize, artifact.FileCount)
	}
	encodedMode, err := strconv.ParseUint(artifact.Mode, 8, 32)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid signed install mode", ErrInvalidManifest)
	}
	entrypoint, err := validateExtractedEntrypoint(staging, artifact.Entrypoint, os.FileMode(encodedMode))
	if err != nil {
		return nil, err
	}
	if err := syncDirectory(staging); err != nil {
		return nil, fmt.Errorf("forgeupdate: sync bundle staging directory: %w", err)
	}
	if err := runHooks(ctx, options.Executor, artifact.Hooks, HookPreflight, staging, manifest.Version); err != nil {
		return nil, err
	}
	// A signed preflight hook may modify its own bundle. Revalidate the
	// executable and persist its final signed mode before the commit point.
	entrypoint, err = validateExtractedEntrypoint(staging, artifact.Entrypoint, os.FileMode(encodedMode))
	if err != nil {
		return nil, err
	}
	if err := syncDirectory(staging); err != nil {
		return nil, fmt.Errorf("forgeupdate: sync preflight bundle staging directory: %w", err)
	}
	if err := os.Rename(staging, versionPath); err != nil {
		if _, statErr := os.Lstat(versionPath); statErr == nil {
			return nil, fmt.Errorf("%w: %s", ErrAlreadyInstalled, manifest.Version)
		}
		return nil, fmt.Errorf("forgeupdate: commit version directory: %w", err)
	}
	stagingPresent = false
	if err := syncDirectory(versions); err != nil {
		rollbackErr := removeBundleVersion(versionPath)
		if rollbackErr != nil {
			return nil, fmt.Errorf("forgeupdate: sync committed version directory: %v; rollback failed: %w", err, rollbackErr)
		}
		return nil, fmt.Errorf("forgeupdate: sync committed version directory: %w", err)
	}
	relativeEntrypoint, err := filepath.Rel(staging, entrypoint)
	if err != nil {
		rollbackErr := removeBundleVersion(versionPath)
		if rollbackErr != nil {
			return nil, fmt.Errorf("forgeupdate: resolve committed entrypoint: %v; rollback failed: %w", err, rollbackErr)
		}
		return nil, fmt.Errorf("forgeupdate: resolve committed entrypoint: %w", err)
	}
	committedEntrypoint := filepath.Join(versionPath, relativeEntrypoint)
	pointer, switched, switchErr := switchBundlePointer(root, versionPath)
	receipt := &BundleReceipt{
		versionPath: versionPath,
		entrypoint:  committedEntrypoint,
		activeEntrypoint: filepath.Join(
			root, "current", relativeEntrypoint,
		),
		pointer: pointer,
		state:   &bundleReceiptState{active: switched},
	}
	if switchErr != nil {
		if switched {
			rollbackErr := receipt.Rollback()
			if rollbackErr != nil {
				return nil, fmt.Errorf("%v; rollback failed: %w", switchErr, rollbackErr)
			}
			return nil, switchErr
		}
		rollbackErr := removeBundleVersion(versionPath)
		if rollbackErr != nil {
			return nil, fmt.Errorf("%v; rollback failed: %w", switchErr, rollbackErr)
		}
		return nil, switchErr
	}
	if err := runHooks(ctx, options.Executor, artifact.Hooks, HookPostInstall, versionPath, manifest.Version); err != nil {
		rollbackErr := receipt.Rollback()
		if rollbackErr != nil {
			return nil, fmt.Errorf("%v; rollback failed: %w", err, rollbackErr)
		}
		return nil, err
	}
	if err := runHooks(ctx, options.Executor, artifact.Hooks, HookVerify, versionPath, manifest.Version); err != nil {
		rollbackErr := receipt.Rollback()
		if rollbackErr != nil {
			return nil, fmt.Errorf("%v; rollback failed: %w", err, rollbackErr)
		}
		return nil, err
	}
	return receipt, nil
}

// Rollback first restores the previous current link (or removes current when
// this was the first install), then removes the version created by the
// receipt. It never removes a version while current still points to it.
func (receipt *BundleReceipt) Rollback() error {
	if receipt == nil || receipt.state == nil {
		return nil
	}
	receipt.state.mu.Lock()
	defer receipt.state.mu.Unlock()
	if !receipt.state.active {
		return nil
	}
	if !receipt.state.pointerRestored {
		restored, err := restoreBundlePointer(receipt.pointer)
		if restored {
			receipt.state.pointerRestored = true
		}
		if err != nil {
			return err
		}
	}
	// Repeat the root fsync even when restoreBundlePointer already completed
	// one. This makes a retry after an earlier fsync failure durable before the
	// active version directory can be removed.
	if err := syncDirectory(receipt.pointer.root); err != nil {
		return fmt.Errorf("forgeupdate: sync restored bundle pointer: %w", err)
	}
	if err := removeBundleVersion(receipt.versionPath); err != nil {
		return err
	}
	receipt.state.active = false
	return nil
}

func removeBundleVersion(versionPath string) error {
	if err := os.RemoveAll(versionPath); err != nil {
		return fmt.Errorf("forgeupdate: remove committed version: %w", err)
	}
	if err := syncDirectory(filepath.Dir(versionPath)); err != nil {
		return fmt.Errorf("forgeupdate: sync bundle rollback: %w", err)
	}
	return nil
}

// Finalize confirms that current still points to this receipt's version and
// relinquishes its rollback state. It does not garbage-collect older versions.
func (receipt *BundleReceipt) Finalize() error {
	if receipt == nil || receipt.state == nil {
		return nil
	}
	receipt.state.mu.Lock()
	defer receipt.state.mu.Unlock()
	if !receipt.state.active {
		return nil
	}
	if receipt.state.pointerRestored {
		return fmt.Errorf("%w: bundle pointer was already restored", ErrActivationChanged)
	}
	if err := verifyBundlePointer(receipt.pointer); err != nil {
		return err
	}
	receipt.state.active = false
	return nil
}
