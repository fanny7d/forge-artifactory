package forgeupdate

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCurrentUserExecutorUsesArgumentArrayAndRestrictedEnvironment(t *testing.T) {
	if os.Getenv("FORGEUPDATE_TEST_HELPER") == "1" {
		if os.Getenv("FORGEUPDATE_SECRET") != "" {
			t.Fatal("unapproved environment variable reached hook")
		}
		if os.Getenv("FORGEUPDATE_HOOK_PHASE") != string(HookPreflight) ||
			os.Getenv("FORGEUPDATE_VERSION") != "1.2.0" {
			t.Fatalf("hook environment phase=%q version=%q",
				os.Getenv("FORGEUPDATE_HOOK_PHASE"), os.Getenv("FORGEUPDATE_VERSION"))
		}
		return
	}
	t.Setenv("FORGEUPDATE_SECRET", "must-not-be-inherited")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executor := CurrentUserExecutor{ExtraEnv: []string{"FORGEUPDATE_TEST_HELPER=1"}}
	err = executor.Execute(t.Context(), HookInvocation{
		Root: filepath.Dir(executable), Executable: executable, Version: "1.2.0",
		Hook: Hook{
			Phase: HookPreflight, Path: filepath.Base(executable), TimeoutSeconds: 30,
			Args: []string{"-test.run=^TestCurrentUserExecutorUsesArgumentArrayAndRestrictedEnvironment$"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCurrentUserExecutorRejectsExecutableOutsideRoot(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	err = (CurrentUserExecutor{}).Execute(t.Context(), HookInvocation{
		Root: t.TempDir(), Executable: executable, Version: "1.2.0",
		Hook: Hook{Phase: HookPreflight, Path: "hook", TimeoutSeconds: 30},
	})
	if !errors.Is(err, ErrHookFailed) {
		t.Fatalf("Execute() error = %v", err)
	}
}
