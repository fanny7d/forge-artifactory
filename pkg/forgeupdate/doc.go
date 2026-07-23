// Package forgeupdate verifies signed Forge release manifests and installs
// current-user CLI updates without depending on the repository server's
// internal API types.
//
// HTTPSource implements Forge's product-scoped install-key resolve and
// download endpoints without sending bearer credentials, cookies, or
// following redirects. Client.Check returns an immutable Plan without
// downloading an artifact, so the host CLI retains complete control of
// prompting. Client.Apply opens the artifact only after consent and dispatches
// to the signed self-replace or bundle strategy. Bundle activation atomically
// switches a stable Root/current symbolic link on Unix.
//
// Verifier turns exact signed manifest bytes into an immutable
// VerifiedRelease. InstallSelfBinary and InstallBundle accept only that
// verified value, stream the selected artifact through bounded size and
// SHA-256 checks, run signed argument-array hooks without a shell, and retain
// enough state for rollback.
//
// Self-replacement currently supports POSIX-style executable replacement.
// Windows returns ErrHelperRequired until an out-of-process helper is
// integrated.
package forgeupdate
