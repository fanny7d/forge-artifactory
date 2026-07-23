//go:build !windows

package forgeupdate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func ensureBundlePlatform() error {
	return nil
}

func switchBundlePointer(root, versionPath string) (bundlePointer, bool, error) {
	relative, err := filepath.Rel(root, versionPath)
	if err != nil || relative == "." || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return bundlePointer{}, false, fmt.Errorf("forgeupdate: version path escapes bundle root")
	}
	pointer := bundlePointer{
		root: root, currentPath: filepath.Join(root, "current"), newTarget: relative,
	}
	if err := validateBundleLinkTarget(root, pointer.newTarget); err != nil {
		return bundlePointer{}, false, err
	}
	info, err := os.Lstat(pointer.currentPath)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink == 0 {
			return bundlePointer{}, false, fmt.Errorf("forgeupdate: bundle current path must be a symbolic link")
		}
		pointer.previousTarget, err = os.Readlink(pointer.currentPath)
		if err != nil {
			return bundlePointer{}, false, fmt.Errorf("forgeupdate: read current bundle link: %w", err)
		}
		if err := validateBundleLinkTarget(root, pointer.previousTarget); err != nil {
			return bundlePointer{}, false, fmt.Errorf("forgeupdate: current bundle link is invalid: %w", err)
		}
		pointer.previousExists = true
	case os.IsNotExist(err):
	default:
		return bundlePointer{}, false, fmt.Errorf("forgeupdate: inspect current bundle link: %w", err)
	}
	if err := replaceSymlink(root, pointer.currentPath, pointer.newTarget); err != nil {
		return bundlePointer{}, false, err
	}
	if err := syncDirectory(root); err != nil {
		return pointer, true, fmt.Errorf("forgeupdate: sync current bundle link: %w", err)
	}
	return pointer, true, nil
}

// restoreBundlePointer reports whether the pointer replacement itself
// completed. A true result with a non-nil error means only the directory sync
// failed, so a retry may continue with version removal.
func restoreBundlePointer(pointer bundlePointer) (bool, error) {
	if err := verifyBundlePointer(pointer); err != nil {
		return false, err
	}
	if pointer.previousExists {
		if err := validateBundleLinkTarget(pointer.root, pointer.previousTarget); err != nil {
			return false, fmt.Errorf("forgeupdate: previous bundle target is no longer valid: %w", err)
		}
		if err := replaceSymlink(pointer.root, pointer.currentPath, pointer.previousTarget); err != nil {
			return false, err
		}
		if err := syncDirectory(pointer.root); err != nil {
			return true, fmt.Errorf("forgeupdate: sync restored bundle link: %w", err)
		}
		return true, nil
	}
	if err := os.Remove(pointer.currentPath); err != nil {
		return false, fmt.Errorf("forgeupdate: remove current bundle link: %w", err)
	}
	if err := syncDirectory(pointer.root); err != nil {
		return true, fmt.Errorf("forgeupdate: sync removed bundle link: %w", err)
	}
	return true, nil
}

func verifyBundlePointer(pointer bundlePointer) error {
	info, err := os.Lstat(pointer.currentPath)
	if err != nil {
		return fmt.Errorf("%w: inspect current link: %v", ErrActivationChanged, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("%w: current is not a symbolic link", ErrActivationChanged)
	}
	target, err := os.Readlink(pointer.currentPath)
	if err != nil {
		return fmt.Errorf("%w: read current link: %v", ErrActivationChanged, err)
	}
	if target != pointer.newTarget {
		return fmt.Errorf("%w: current points to %q instead of %q", ErrActivationChanged, target, pointer.newTarget)
	}
	if err := validateBundleLinkTarget(pointer.root, target); err != nil {
		return fmt.Errorf("%w: current target is invalid: %v", ErrActivationChanged, err)
	}
	return nil
}

func replaceSymlink(root, destination, target string) error {
	staging, err := os.MkdirTemp(root, ".forgeupdate-current-*")
	if err != nil {
		return fmt.Errorf("forgeupdate: create current-link staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	candidate := filepath.Join(staging, "current")
	if err := os.Symlink(target, candidate); err != nil {
		return fmt.Errorf("forgeupdate: create current-link candidate: %w", err)
	}
	if err := syncDirectory(staging); err != nil {
		return fmt.Errorf("forgeupdate: sync current-link candidate: %w", err)
	}
	if err := os.Rename(candidate, destination); err != nil {
		return fmt.Errorf("forgeupdate: atomically switch current bundle link: %w", err)
	}
	return nil
}

func validateBundleLinkTarget(root, target string) error {
	if target == "" || filepath.IsAbs(target) || strings.Contains(target, "\\") ||
		filepath.Clean(target) != target {
		return fmt.Errorf("link target %q is not canonical and relative", target)
	}
	parts := strings.Split(filepath.ToSlash(target), "/")
	if len(parts) != 2 || parts[0] != "versions" {
		return fmt.Errorf("link target %q is outside versions", target)
	}
	if _, err := parseSemanticVersion(parts[1]); err != nil {
		return fmt.Errorf("link target version is invalid: %w", err)
	}
	targetPath := filepath.Join(root, target)
	if err := ensureWithinRoot(root, targetPath); err != nil {
		return err
	}
	info, err := os.Lstat(targetPath)
	if err != nil {
		return fmt.Errorf("inspect link target: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("link target must be a real version directory")
	}
	return nil
}
