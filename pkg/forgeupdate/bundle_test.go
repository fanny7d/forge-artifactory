package forgeupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

type testArchiveEntry struct {
	name     string
	content  []byte
	mode     os.FileMode
	typeflag byte
	linkname string
}

func TestInstallBundleTarGzAndZIP(t *testing.T) {
	requireBundlePlatform(t)
	for _, format := range []ArchiveFormat{ArchiveTarGz, ArchiveZIP} {
		t.Run(string(format), func(t *testing.T) {
			entries := []testArchiveEntry{
				{name: "bin/edgecli", content: []byte("executable"), mode: 0o755, typeflag: tar.TypeReg},
				{name: "bin/check", content: []byte("hook"), mode: 0o755, typeflag: tar.TypeReg},
				{name: "share/config.json", content: []byte("{}"), mode: 0o644, typeflag: tar.TypeReg},
			}
			archive := buildArchive(t, format, entries)
			root := t.TempDir()
			executor := &recordingExecutor{onExecute: func(invocation HookInvocation) {
				_, err := os.Lstat(filepath.Join(root, "current"))
				if invocation.Hook.Phase == HookPreflight {
					if !os.IsNotExist(err) {
						t.Fatalf("current exists during preflight: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("current missing during %s: %v", invocation.Hook.Phase, err)
				}
				assertCurrentLink(t, root, "versions/1.2.0")
			}}
			release := bundleReleaseForTest(archive, format, entries, []Hook{
				{Phase: HookPreflight, Path: "bin/check", Args: []string{"--pre"}, TimeoutSeconds: 5},
				{Phase: HookPostInstall, Path: "bin/check", Args: []string{"--post"}, TimeoutSeconds: 5},
				{Phase: HookVerify, Path: "bin/check", Args: []string{"--verify"}, TimeoutSeconds: 5},
			})
			if format == ArchiveZIP {
				release.artifact.UnpackedSize = 0
				release.artifact.FileCount = 0
			}
			receipt, err := InstallBundle(t.Context(), bytes.NewReader(archive), release, BundleOptions{
				Root: root, Executor: executor,
			})
			if err != nil {
				t.Fatal(err)
			}
			assertFileContent(t, receipt.Entrypoint(), []byte("executable"))
			assertFileContent(t, receipt.ActiveEntrypoint(), []byte("executable"))
			if receipt.ActiveEntrypoint() != filepath.Join(root, "current", "bin", "edgecli") {
				t.Fatalf("ActiveEntrypoint() = %q", receipt.ActiveEntrypoint())
			}
			assertCurrentLink(t, root, "versions/1.2.0")
			assertFileContent(t, filepath.Join(receipt.VersionPath(), "share", "config.json"), []byte("{}"))
			if len(executor.invocations) != 3 {
				t.Fatalf("hook invocations = %+v", executor.invocations)
			}
			for index, phase := range []HookPhase{HookPreflight, HookPostInstall, HookVerify} {
				invocation := executor.invocations[index]
				if invocation.Hook.Phase != phase || filepath.Base(invocation.Executable) != "check" {
					t.Fatalf("hook invocation %d = %+v", index, invocation)
				}
			}
			versionPath := receipt.VersionPath()
			if err := receipt.Rollback(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(versionPath); !os.IsNotExist(err) {
				t.Fatalf("version directory still exists after rollback: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(root, "current")); !os.IsNotExist(err) {
				t.Fatalf("current still exists after first-install rollback: %v", err)
			}
		})
	}
}

func TestInstallBundleRejectsUnsafeArchives(t *testing.T) {
	requireBundlePlatform(t)
	tests := []struct {
		name    string
		format  ArchiveFormat
		entries []testArchiveEntry
	}{
		{
			name: "tar traversal", format: ArchiveTarGz,
			entries: []testArchiveEntry{{name: "../edgecli", content: []byte("x"), mode: 0o755, typeflag: tar.TypeReg}},
		},
		{
			name: "tar symlink", format: ArchiveTarGz,
			entries: []testArchiveEntry{{name: "edgecli", mode: 0o777, typeflag: tar.TypeSymlink, linkname: "/bin/sh"}},
		},
		{
			name: "tar hardlink", format: ArchiveTarGz,
			entries: []testArchiveEntry{{name: "edgecli", mode: 0o777, typeflag: tar.TypeLink, linkname: "other"}},
		},
		{
			name: "tar duplicate", format: ArchiveTarGz,
			entries: []testArchiveEntry{
				{name: "edgecli", content: []byte("a"), mode: 0o755, typeflag: tar.TypeReg},
				{name: "edgecli", content: []byte("b"), mode: 0o755, typeflag: tar.TypeReg},
			},
		},
		{
			name: "zip traversal", format: ArchiveZIP,
			entries: []testArchiveEntry{{name: "../edgecli", content: []byte("x"), mode: 0o755}},
		},
		{
			name: "zip symlink", format: ArchiveZIP,
			entries: []testArchiveEntry{{name: "edgecli", content: []byte("target"), mode: os.ModeSymlink | 0o777}},
		},
		{
			name: "zip duplicate", format: ArchiveZIP,
			entries: []testArchiveEntry{
				{name: "edgecli", content: []byte("a"), mode: 0o755},
				{name: "edgecli", content: []byte("b"), mode: 0o755},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			archive := buildArchive(t, test.format, test.entries)
			release := bundleReleaseForTest(archive, test.format, test.entries, nil)
			// The attack may make the declared entrypoint absent. Extraction
			// must still fail as an unsafe archive before any commit.
			_, err := InstallBundle(t.Context(), bytes.NewReader(archive), release, BundleOptions{Root: t.TempDir()})
			if !errors.Is(err, ErrUnsafeArchive) {
				t.Fatalf("InstallBundle() error = %v", err)
			}
		})
	}
}

func TestInstallBundleLimitsMissingEntrypointAndHookRollback(t *testing.T) {
	requireBundlePlatform(t)
	entries := []testArchiveEntry{
		{name: "bin/edgecli", content: []byte("executable"), mode: 0o755, typeflag: tar.TypeReg},
		{name: "bin/check", content: []byte("hook"), mode: 0o755, typeflag: tar.TypeReg},
	}
	archive := buildArchive(t, ArchiveTarGz, entries)
	release := bundleReleaseForTest(archive, ArchiveTarGz, entries, nil)
	if _, err := InstallBundle(t.Context(), bytes.NewReader(archive), release, BundleOptions{
		Root: t.TempDir(), MaxUnpackedBytes: int64(len(entries[0].content) - 1),
	}); !errors.Is(err, ErrUnsafeArchive) {
		t.Fatalf("unpacked limit error = %v", err)
	}

	missing := release
	missing.artifact.Entrypoint = "bin/missing"
	if _, err := InstallBundle(t.Context(), bytes.NewReader(archive), missing, BundleOptions{Root: t.TempDir()}); !errors.Is(err, ErrUnsafeArchive) {
		t.Fatalf("missing entrypoint error = %v", err)
	}

	for _, phase := range []HookPhase{HookPreflight, HookPostInstall, HookVerify} {
		t.Run(string(phase), func(t *testing.T) {
			root := t.TempDir()
			prepareBundleVersion(t, root, "1.1.0", []byte("old executable"))
			failing := release
			failing.artifact.Hooks = []Hook{{
				Phase: phase, Path: "bin/check", Args: []string{"--version"}, TimeoutSeconds: 5,
			}}
			_, err := InstallBundle(t.Context(), bytes.NewReader(archive), failing, BundleOptions{
				Root: root, Executor: &recordingExecutor{failPhase: phase},
			})
			if !errors.Is(err, ErrHookFailed) {
				t.Fatalf("%s hook error = %v", phase, err)
			}
			if _, statErr := os.Stat(filepath.Join(root, "versions", failing.manifest.Version)); !os.IsNotExist(statErr) {
				t.Fatalf("failed version remains committed: %v", statErr)
			}
			assertCurrentLink(t, root, "versions/1.1.0")
			assertFileContent(t, filepath.Join(root, "current", "bin", "edgecli"), []byte("old executable"))
		})
	}
}

func TestBundleReceiptRestoresPreviousCurrentAndFinalizeKeepsNewCurrent(t *testing.T) {
	requireBundlePlatform(t)
	entries := []testArchiveEntry{
		{name: "bin/edgecli", content: []byte("new executable"), mode: 0o755, typeflag: tar.TypeReg},
	}
	archive := buildArchive(t, ArchiveTarGz, entries)
	release := bundleReleaseForTest(archive, ArchiveTarGz, entries, nil)

	t.Run("rollback", func(t *testing.T) {
		root := t.TempDir()
		prepareBundleVersion(t, root, "1.1.0", []byte("old executable"))
		receipt, err := InstallBundle(
			t.Context(), bytes.NewReader(archive), release, BundleOptions{Root: root},
		)
		if err != nil {
			t.Fatal(err)
		}
		assertCurrentLink(t, root, "versions/1.2.0")
		assertFileContent(t, receipt.ActiveEntrypoint(), []byte("new executable"))
		if err := receipt.Rollback(); err != nil {
			t.Fatal(err)
		}
		assertCurrentLink(t, root, "versions/1.1.0")
		assertFileContent(t, receipt.ActiveEntrypoint(), []byte("old executable"))
		if _, err := os.Stat(receipt.VersionPath()); !os.IsNotExist(err) {
			t.Fatalf("new version remains after rollback: %v", err)
		}
	})

	t.Run("finalize", func(t *testing.T) {
		root := t.TempDir()
		prepareBundleVersion(t, root, "1.1.0", []byte("old executable"))
		receipt, err := InstallBundle(
			t.Context(), bytes.NewReader(archive), release, BundleOptions{Root: root},
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := receipt.Finalize(); err != nil {
			t.Fatal(err)
		}
		if err := receipt.Rollback(); err != nil {
			t.Fatal(err)
		}
		assertCurrentLink(t, root, "versions/1.2.0")
		assertFileContent(t, receipt.ActiveEntrypoint(), []byte("new executable"))
		if _, err := os.Stat(receipt.VersionPath()); err != nil {
			t.Fatalf("finalized version is missing: %v", err)
		}
	})
}

func TestInstallBundleRejectsInvalidCurrentAndRollbackDoesNotClobberNewerActivation(t *testing.T) {
	requireBundlePlatform(t)
	entries := []testArchiveEntry{
		{name: "bin/edgecli", content: []byte("new executable"), mode: 0o755, typeflag: tar.TypeReg},
	}
	archive := buildArchive(t, ArchiveTarGz, entries)
	release := bundleReleaseForTest(archive, ArchiveTarGz, entries, nil)
	tests := []struct {
		name    string
		prepare func(*testing.T, string)
	}{
		{
			name: "regular file",
			prepare: func(t *testing.T, root string) {
				if err := os.WriteFile(filepath.Join(root, "current"), []byte("not a link"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "absolute link",
			prepare: func(t *testing.T, root string) {
				if err := os.Symlink(filepath.Join(root, "versions", "1.1.0"), filepath.Join(root, "current")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "dangling link",
			prepare: func(t *testing.T, root string) {
				if err := os.Symlink("versions/1.1.0", filepath.Join(root, "current")); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "versions"), 0o755); err != nil {
				t.Fatal(err)
			}
			test.prepare(t, root)
			_, err := InstallBundle(
				t.Context(), bytes.NewReader(archive), release, BundleOptions{Root: root},
			)
			if err == nil {
				t.Fatal("InstallBundle() accepted invalid current")
			}
			if _, statErr := os.Stat(filepath.Join(root, "versions", "1.2.0")); !os.IsNotExist(statErr) {
				t.Fatalf("new version remains after activation rejection: %v", statErr)
			}
		})
	}

	root := t.TempDir()
	prepareBundleVersion(t, root, "1.1.0", []byte("old executable"))
	receipt, err := InstallBundle(
		t.Context(), bytes.NewReader(archive), release, BundleOptions{Root: root},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "versions", "1.3.0", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("versions/1.3.0", filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	if err := receipt.Rollback(); !errors.Is(err, ErrActivationChanged) {
		t.Fatalf("Rollback() error = %v", err)
	}
	assertCurrentLink(t, root, "versions/1.3.0")
	if _, err := os.Stat(receipt.VersionPath()); err != nil {
		t.Fatalf("receipt version was removed after activation changed: %v", err)
	}
}

func TestInstallBundleWindowsRequiresHelperBeforeReadingOrWriting(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-only contract")
	}
	root := filepath.Join(t.TempDir(), "must-not-exist")
	release := VerifiedRelease{
		manifest: Manifest{Version: "1.2.0"},
		artifact: Artifact{Kind: ArtifactBundle},
	}
	_, err := InstallBundle(t.Context(), panicReader{}, release, BundleOptions{Root: root})
	if !errors.Is(err, ErrHelperRequired) {
		t.Fatalf("InstallBundle() error = %v", err)
	}
	if _, statErr := os.Stat(root); !os.IsNotExist(statErr) {
		t.Fatalf("Windows bundle install wrote root: %v", statErr)
	}
}

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) {
	panic("bundle source was read before the Windows helper check")
}

var _ io.Reader = panicReader{}

func requireBundlePlatform(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows bundle activation requires an external helper")
	}
}

func prepareBundleVersion(t *testing.T, root, version string, content []byte) {
	t.Helper()
	entrypoint := filepath.Join(root, "versions", version, "bin", "edgecli")
	if err := os.MkdirAll(filepath.Dir(entrypoint), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entrypoint, content, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("versions", version), filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
}

func assertCurrentLink(t *testing.T, root, expected string) {
	t.Helper()
	actual, err := os.Readlink(filepath.Join(root, "current"))
	if err != nil {
		t.Fatalf("read current link: %v", err)
	}
	if actual != expected {
		t.Fatalf("current link = %q, want %q", actual, expected)
	}
}

func buildArchive(t *testing.T, format ArchiveFormat, entries []testArchiveEntry) []byte {
	t.Helper()
	var encoded bytes.Buffer
	switch format {
	case ArchiveTarGz:
		compressed := gzip.NewWriter(&encoded)
		writer := tar.NewWriter(compressed)
		for _, entry := range entries {
			typeflag := entry.typeflag
			if typeflag == 0 {
				typeflag = tar.TypeReg
			}
			header := &tar.Header{
				Name: entry.name, Mode: int64(entry.mode.Perm()), Size: int64(len(entry.content)),
				Typeflag: typeflag, Linkname: entry.linkname,
			}
			if typeflag != tar.TypeReg && typeflag != tar.TypeRegA {
				header.Size = 0
			}
			if err := writer.WriteHeader(header); err != nil {
				t.Fatal(err)
			}
			if header.Size > 0 {
				if _, err := writer.Write(entry.content); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if err := compressed.Close(); err != nil {
			t.Fatal(err)
		}
	case ArchiveZIP:
		writer := zip.NewWriter(&encoded)
		for _, entry := range entries {
			header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
			header.SetMode(entry.mode)
			file, err := writer.CreateHeader(header)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.Write(entry.content); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unsupported test archive format %q", format)
	}
	return encoded.Bytes()
}

func bundleReleaseForTest(encoded []byte, format ArchiveFormat, entries []testArchiveEntry, hooks []Hook) VerifiedRelease {
	artifact := artifactForBytes(encoded)
	artifact.Kind = ArtifactBundle
	artifact.Strategy = InstallStrategyBundle
	artifact.Format = format
	artifact.Entrypoint = "bin/edgecli"
	artifact.Mode = "0755"
	artifact.Hooks = hooks
	for _, entry := range entries {
		typeflag := entry.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		regular := format == ArchiveZIP && entry.mode&os.ModeType == 0
		if format == ArchiveTarGz {
			regular = typeflag == tar.TypeReg || typeflag == tar.TypeRegA
		}
		if regular {
			artifact.FileCount++
			artifact.UnpackedSize += int64(len(entry.content))
		}
	}
	return VerifiedRelease{
		manifest: Manifest{Version: "1.2.0"},
		artifact: artifact,
	}
}
