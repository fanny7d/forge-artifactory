package forgeupdate

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
)

type SelfBinaryOptions struct {
	// Target is required. Callers should resolve their intended installation
	// path explicitly instead of relying on argv[0].
	Target           string
	MaxDownloadBytes int64
	// Probe must start or otherwise validate the staged candidate at Path.
	// InstallSelfBinary refuses to replace Target when Probe is nil.
	Probe CandidateProbe
}

type Candidate struct {
	Path     string
	Version  string
	Artifact Artifact
}

type CandidateProbe func(context.Context, Candidate) error

type SelfBinaryReceipt struct {
	target     string
	backupPath string
	active     bool
}

func (receipt *SelfBinaryReceipt) Target() string {
	if receipt == nil {
		return ""
	}
	return receipt.target
}

func (receipt *SelfBinaryReceipt) BackupPath() string {
	if receipt == nil {
		return ""
	}
	return receipt.backupPath
}

// InstallSelfBinary installs a verified binary with a same-directory staging
// file and rollback copy. Windows intentionally returns ErrHelperRequired:
// replacing an executing .exe needs an out-of-process helper.
func InstallSelfBinary(ctx context.Context, source io.Reader, release VerifiedRelease, options SelfBinaryOptions) (*SelfBinaryReceipt, error) {
	if runtime.GOOS == "windows" {
		return nil, ErrHelperRequired
	}
	artifact := release.artifact
	manifest := release.manifest
	if artifact.Kind != ArtifactBinary {
		return nil, fmt.Errorf("forgeupdate: self installer requires a binary artifact")
	}
	if len(artifact.Hooks) != 0 {
		return nil, fmt.Errorf("forgeupdate: self-replace artifacts must not contain manifest hooks")
	}
	if options.Probe == nil {
		return nil, ErrProbeRequired
	}
	if _, err := parseSemanticVersion(manifest.Version); err != nil {
		return nil, fmt.Errorf("forgeupdate: invalid release version: %w", err)
	}
	if options.Target == "" {
		return nil, fmt.Errorf("forgeupdate: self installer target is required")
	}
	target, err := filepath.Abs(options.Target)
	if err != nil {
		return nil, fmt.Errorf("forgeupdate: resolve target: %w", err)
	}
	targetInfo, err := os.Lstat(target)
	if err != nil {
		return nil, fmt.Errorf("forgeupdate: inspect target: %w", err)
	}
	if !targetInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("forgeupdate: target must be a regular file, not a symlink or special file")
	}
	directory := filepath.Dir(target)
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(target)+".forgeupdate-new-*")
	if err != nil {
		return nil, fmt.Errorf("forgeupdate: create same-directory staging file: %w", err)
	}
	temporaryPath := temporary.Name()
	temporaryPresent := true
	defer func() {
		_ = temporary.Close()
		if temporaryPresent {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := CopyVerified(ctx, temporary, source, artifact, DownloadLimits{MaxBytes: options.MaxDownloadBytes}); err != nil {
		return nil, err
	}
	installMode := targetInfo.Mode().Perm()
	if manifest.SchemaVersion == 2 {
		encodedMode, parseErr := strconv.ParseUint(artifact.Mode, 8, 32)
		if parseErr != nil {
			return nil, fmt.Errorf("%w: invalid signed install mode", ErrInvalidManifest)
		}
		installMode = os.FileMode(encodedMode)
	}
	if err := temporary.Chmod(installMode); err != nil {
		return nil, fmt.Errorf("forgeupdate: set executable permissions: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return nil, fmt.Errorf("forgeupdate: sync staged binary: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return nil, fmt.Errorf("forgeupdate: close staged binary: %w", err)
	}
	if err := options.Probe(ctx, Candidate{
		Path: temporaryPath, Version: manifest.Version, Artifact: cloneArtifact(artifact),
	}); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrProbeFailed, err)
	}
	backupPath, err := copyBackup(target, targetInfo.Mode().Perm())
	if err != nil {
		return nil, err
	}
	cleanupBackup := true
	defer func() {
		if cleanupBackup {
			_ = os.Remove(backupPath)
		}
	}()
	if err := os.Rename(temporaryPath, target); err != nil {
		return nil, fmt.Errorf("forgeupdate: atomically replace target: %w", err)
	}
	temporaryPresent = false
	// After the commit point, retain the backup on every ambiguous failure.
	// A successful rollback consumes it by renaming it back into place.
	cleanupBackup = false
	if err := syncDirectory(directory); err != nil {
		rollbackErr := restoreBackup(target, backupPath)
		if rollbackErr != nil {
			return nil, fmt.Errorf("forgeupdate: sync installed binary: %v; rollback failed: %w", err, rollbackErr)
		}
		return nil, fmt.Errorf("forgeupdate: sync installed binary: %w", err)
	}
	return &SelfBinaryReceipt{target: target, backupPath: backupPath, active: true}, nil
}

func copyBackup(target string, mode os.FileMode) (string, error) {
	directory := filepath.Dir(target)
	backup, err := os.CreateTemp(directory, "."+filepath.Base(target)+".forgeupdate-old-*")
	if err != nil {
		return "", fmt.Errorf("forgeupdate: create rollback copy: %w", err)
	}
	backupPath := backup.Name()
	keep := false
	defer func() {
		_ = backup.Close()
		if !keep {
			_ = os.Remove(backupPath)
		}
	}()
	current, err := os.Open(target)
	if err != nil {
		return "", fmt.Errorf("forgeupdate: open current binary for backup: %w", err)
	}
	defer func() { _ = current.Close() }()
	if _, err := io.Copy(backup, current); err != nil {
		return "", fmt.Errorf("forgeupdate: copy current binary to backup: %w", err)
	}
	if err := backup.Chmod(mode); err != nil {
		return "", fmt.Errorf("forgeupdate: preserve backup permissions: %w", err)
	}
	if err := backup.Sync(); err != nil {
		return "", fmt.Errorf("forgeupdate: sync rollback copy: %w", err)
	}
	if err := backup.Close(); err != nil {
		return "", fmt.Errorf("forgeupdate: close rollback copy: %w", err)
	}
	if err := syncDirectory(directory); err != nil {
		return "", fmt.Errorf("forgeupdate: sync rollback copy: %w", err)
	}
	keep = true
	return backupPath, nil
}

func restoreBackup(target, backupPath string) error {
	if err := os.Rename(backupPath, target); err != nil {
		return fmt.Errorf("forgeupdate: restore rollback copy: %w", err)
	}
	if err := syncDirectory(filepath.Dir(target)); err != nil {
		return fmt.Errorf("forgeupdate: sync restored binary: %w", err)
	}
	return nil
}

// Rollback restores the old binary. It is idempotent after a successful
// rollback or Finalize.
func (receipt *SelfBinaryReceipt) Rollback() error {
	if receipt == nil || !receipt.active {
		return nil
	}
	if err := restoreBackup(receipt.target, receipt.backupPath); err != nil {
		return err
	}
	receipt.active = false
	return nil
}

// Finalize removes the rollback copy after the caller has confirmed the new
// binary started successfully.
func (receipt *SelfBinaryReceipt) Finalize() error {
	if receipt == nil || !receipt.active {
		return nil
	}
	if err := os.Remove(receipt.backupPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("forgeupdate: remove rollback copy: %w", err)
	}
	if err := syncDirectory(filepath.Dir(receipt.target)); err != nil {
		return fmt.Errorf("forgeupdate: sync finalized binary: %w", err)
	}
	receipt.active = false
	return nil
}
