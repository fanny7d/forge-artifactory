package forgeupdate

import "errors"

var (
	ErrInvalidManifest   = errors.New("forgeupdate: invalid manifest")
	ErrInvalidSignature  = errors.New("forgeupdate: invalid signature")
	ErrUntrustedKey      = errors.New("forgeupdate: untrusted signing key")
	ErrNoUpdate          = errors.New("forgeupdate: no newer version")
	ErrDownloadTooLarge  = errors.New("forgeupdate: download exceeds size limit")
	ErrSizeMismatch      = errors.New("forgeupdate: downloaded size mismatch")
	ErrChecksumMismatch  = errors.New("forgeupdate: downloaded checksum mismatch")
	ErrHelperRequired    = errors.New("forgeupdate: platform helper required")
	ErrProbeRequired     = errors.New("forgeupdate: candidate probe is required")
	ErrProbeFailed       = errors.New("forgeupdate: candidate probe failed")
	ErrUnsafeArchive     = errors.New("forgeupdate: unsafe archive")
	ErrAlreadyInstalled  = errors.New("forgeupdate: version already installed")
	ErrHookFailed        = errors.New("forgeupdate: hook failed")
	ErrSource            = errors.New("forgeupdate: source request failed")
	ErrInvalidResponse   = errors.New("forgeupdate: invalid source response")
	ErrHTTPStatus        = errors.New("forgeupdate: unexpected HTTP status")
	ErrInvalidPlan       = errors.New("forgeupdate: invalid update plan")
	ErrPlanUsed          = errors.New("forgeupdate: update plan already used")
	ErrActivationChanged = errors.New("forgeupdate: active bundle changed")
)
