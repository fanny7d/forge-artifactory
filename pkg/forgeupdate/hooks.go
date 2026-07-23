package forgeupdate

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const defaultHookTimeout = 30 * time.Second

type HookInvocation struct {
	Root       string
	Executable string
	Version    string
	Hook       Hook
}

type HookExecutor interface {
	Execute(context.Context, HookInvocation) error
}

// CurrentUserExecutor runs the exact executable with an argument array. It
// never invokes a shell, never changes identity, confines the executable to
// Root, and exposes only a small allowlist of the current user's environment.
type CurrentUserExecutor struct {
	Stdout     io.Writer
	Stderr     io.Writer
	ExtraEnv   []string
	MaxTimeout time.Duration
}

func (executor CurrentUserExecutor) Execute(ctx context.Context, invocation HookInvocation) error {
	if err := validateHook(invocation.Hook); err != nil {
		return fmt.Errorf("%w: %v", ErrHookFailed, err)
	}
	root, err := filepath.Abs(invocation.Root)
	if err != nil {
		return fmt.Errorf("%w: resolve hook root: %v", ErrHookFailed, err)
	}
	executable, err := filepath.Abs(invocation.Executable)
	if err != nil {
		return fmt.Errorf("%w: resolve hook executable: %v", ErrHookFailed, err)
	}
	relative, err := filepath.Rel(root, executable)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return fmt.Errorf("%w: executable escapes hook root", ErrHookFailed)
	}
	info, err := os.Lstat(executable)
	if err != nil {
		return fmt.Errorf("%w: inspect executable: %v", ErrHookFailed, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: executable is not a regular file", ErrHookFailed)
	}
	timeout := defaultHookTimeout
	if invocation.Hook.TimeoutSeconds > 0 {
		timeout = time.Duration(invocation.Hook.TimeoutSeconds) * time.Second
	}
	if executor.MaxTimeout > 0 && timeout > executor.MaxTimeout {
		timeout = executor.MaxTimeout
	}
	hookContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(hookContext, executable, invocation.Hook.Args...)
	command.Dir = root
	command.Env = restrictedEnvironment(executor.ExtraEnv, invocation)
	if executor.Stdout != nil {
		command.Stdout = executor.Stdout
	} else {
		command.Stdout = io.Discard
	}
	if executor.Stderr != nil {
		command.Stderr = executor.Stderr
	} else {
		command.Stderr = io.Discard
	}
	if err := command.Run(); err != nil {
		if hookContext.Err() != nil {
			return fmt.Errorf("%w: %s hook timed out: %v", ErrHookFailed, invocation.Hook.Phase, hookContext.Err())
		}
		return fmt.Errorf("%w: %s hook: %v", ErrHookFailed, invocation.Hook.Phase, err)
	}
	return nil
}

func restrictedEnvironment(extra []string, invocation HookInvocation) []string {
	allowed := map[string]struct{}{
		"HOME": {}, "LOGNAME": {}, "PATH": {}, "PATHEXT": {}, "SHELL": {},
		"SYSTEMROOT": {}, "TEMP": {}, "TMP": {}, "TMPDIR": {}, "USER": {},
		"USERPROFILE": {}, "WINDIR": {},
	}
	result := make([]string, 0, len(allowed)+len(extra)+2)
	for _, encoded := range os.Environ() {
		name, _, ok := strings.Cut(encoded, "=")
		if !ok {
			continue
		}
		key := name
		if runtime.GOOS == "windows" {
			key = strings.ToUpper(name)
		}
		if _, permitted := allowed[key]; permitted {
			result = append(result, encoded)
		}
	}
	for _, encoded := range extra {
		name, _, ok := strings.Cut(encoded, "=")
		if !ok || name == "" || strings.ContainsAny(name, "\x00=") || strings.ContainsRune(encoded, '\x00') {
			continue
		}
		result = append(result, encoded)
	}
	result = append(result,
		"FORGEUPDATE_HOOK_PHASE="+string(invocation.Hook.Phase),
		"FORGEUPDATE_VERSION="+invocation.Version,
	)
	return result
}

func runHooks(ctx context.Context, executor HookExecutor, hooks []Hook, phase HookPhase, root, version string) error {
	if executor == nil {
		executor = CurrentUserExecutor{}
	}
	for _, hook := range hooks {
		if hook.Phase != phase {
			continue
		}
		if err := validateHook(hook); err != nil {
			return fmt.Errorf("%w: %v", ErrHookFailed, err)
		}
		relative, err := safeRelativePath(hook.Path)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrHookFailed, err)
		}
		executable := filepath.Join(root, relative)
		if err := ensureWithinRoot(root, executable); err != nil {
			return fmt.Errorf("%w: %v", ErrHookFailed, err)
		}
		if err := executor.Execute(ctx, HookInvocation{
			Root: root, Executable: executable, Version: version, Hook: hook,
		}); err != nil {
			return err
		}
	}
	return nil
}
