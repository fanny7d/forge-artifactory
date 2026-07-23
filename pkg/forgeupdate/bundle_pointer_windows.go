//go:build windows

package forgeupdate

func ensureBundlePlatform() error {
	return ErrHelperRequired
}

func switchBundlePointer(string, string) (bundlePointer, bool, error) {
	return bundlePointer{}, false, ErrHelperRequired
}

func restoreBundlePointer(bundlePointer) (bool, error) {
	return false, ErrHelperRequired
}

func verifyBundlePointer(bundlePointer) error {
	return ErrHelperRequired
}
