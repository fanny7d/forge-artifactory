package forgeupdate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

type recordingExecutor struct {
	invocations []HookInvocation
	failPhase   HookPhase
	onExecute   func(HookInvocation)
}

func (executor *recordingExecutor) Execute(_ context.Context, invocation HookInvocation) error {
	executor.invocations = append(executor.invocations, invocation)
	if executor.onExecute != nil {
		executor.onExecute(invocation)
	}
	if invocation.Hook.Phase == executor.failPhase {
		return ErrHookFailed
	}
	return nil
}

func TestInstallSelfBinaryCommitAndRollback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows V1 deliberately requires an external helper")
	}
	directory := t.TempDir()
	target := filepath.Join(directory, "edgecli")
	oldContent := []byte("old executable")
	newContent := []byte("new executable")
	if err := os.WriteFile(target, oldContent, 0o751); err != nil {
		t.Fatal(err)
	}
	release := selfReleaseForTest(newContent)
	var candidates []Candidate
	receipt, err := InstallSelfBinary(t.Context(), bytes.NewReader(newContent), release, SelfBinaryOptions{
		Target: target,
		Probe: func(_ context.Context, candidate Candidate) error {
			candidates = append(candidates, candidate)
			assertFileContent(t, candidate.Path, newContent)
			if candidate.Version != "1.2.0" || candidate.Artifact.SHA256 != release.artifact.SHA256 {
				t.Fatalf("candidate = %+v", candidate)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, target, newContent)
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o751 {
		t.Fatalf("installed mode = %o, want 751", info.Mode().Perm())
	}
	if len(candidates) != 1 {
		t.Fatalf("probe candidates = %+v", candidates)
	}
	assertFileContent(t, receipt.BackupPath(), oldContent)
	if err := receipt.Rollback(); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, target, oldContent)
	if _, err := os.Stat(receipt.BackupPath()); !os.IsNotExist(err) {
		t.Fatalf("backup still exists after rollback: %v", err)
	}
}

func TestInstallSelfBinaryRequiresSuccessfulProbeBeforeReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows V1 deliberately requires an external helper")
	}
	probeFailure := errors.New("candidate did not start")
	tests := []struct {
		name    string
		probe   CandidateProbe
		wantErr error
	}{
		{name: "missing", wantErr: ErrProbeRequired},
		{
			name: "failure",
			probe: func(context.Context, Candidate) error {
				return probeFailure
			},
			wantErr: ErrProbeFailed,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			target := filepath.Join(directory, "edgecli")
			oldContent := []byte("old")
			newContent := []byte("new")
			if err := os.WriteFile(target, oldContent, 0o755); err != nil {
				t.Fatal(err)
			}
			_, err := InstallSelfBinary(
				t.Context(),
				bytes.NewReader(newContent),
				selfReleaseForTest(newContent),
				SelfBinaryOptions{Target: target, Probe: test.probe},
			)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("InstallSelfBinary() error = %v", err)
			}
			assertFileContent(t, target, oldContent)
		})
	}
}

func TestSelfBinaryFinalizeRemovesBackup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows V1 deliberately requires an external helper")
	}
	target := filepath.Join(t.TempDir(), "edgecli")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("new")
	receipt, err := InstallSelfBinary(
		t.Context(),
		bytes.NewReader(content),
		selfReleaseForTest(content),
		SelfBinaryOptions{Target: target, Probe: func(context.Context, Candidate) error { return nil }},
	)
	if err != nil {
		t.Fatal(err)
	}
	backupPath := receipt.BackupPath()
	if err := receipt.Finalize(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(backupPath); !os.IsNotExist(err) {
		t.Fatalf("backup still exists after finalize: %v", err)
	}
}

func selfReleaseForTest(content []byte) VerifiedRelease {
	artifact := artifactForBytes(content)
	artifact.Kind = ArtifactBinary
	return VerifiedRelease{
		manifest: Manifest{Version: "1.2.0"},
		artifact: artifact,
	}
}

func assertFileContent(t *testing.T, path string, expected []byte) {
	t.Helper()
	actual, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatalf("%s content = %q, want %q", path, actual, expected)
	}
}
