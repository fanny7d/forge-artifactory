package release

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
)

type InstallStrategy string

const (
	InstallStrategySelfReplace InstallStrategy = "self-replace"
	InstallStrategyBundle      InstallStrategy = "bundle"
)

type InstallFormat string

const (
	InstallFormatRaw   InstallFormat = "raw"
	InstallFormatTarGZ InstallFormat = "tar.gz"
	InstallFormatZIP   InstallFormat = "zip"
)

type HookPhase string

const (
	HookPhasePreflight   HookPhase = "preflight"
	HookPhasePostInstall HookPhase = "post-install"
	HookPhaseVerify      HookPhase = "verify"
)

type InstallHook struct {
	Phase          HookPhase `json:"phase"`
	Path           string    `json:"path"`
	Args           []string  `json:"args,omitempty"`
	TimeoutSeconds int       `json:"timeoutSeconds"`
}

type InstallSpec struct {
	Strategy   InstallStrategy `json:"strategy"`
	Format     InstallFormat   `json:"format"`
	Entrypoint string          `json:"entrypoint,omitempty"`
	Mode       string          `json:"mode"`
	Hooks      []InstallHook   `json:"hooks,omitempty"`
}

var installModePattern = regexp.MustCompile(`^0[0-7]{3}$`)

func (spec InstallSpec) Validate() error {
	if !installModePattern.MatchString(spec.Mode) {
		return fmt.Errorf("%w: install mode must be a four-digit octal string", ErrInvalidRequest)
	}
	mode, err := strconv.ParseUint(spec.Mode, 8, 32)
	if err != nil || mode&0o111 == 0 || mode > 0o777 {
		return fmt.Errorf("%w: install mode must include an executable bit", ErrInvalidRequest)
	}
	switch spec.Strategy {
	case InstallStrategySelfReplace:
		if spec.Format != InstallFormatRaw || spec.Entrypoint != "" || len(spec.Hooks) != 0 {
			return fmt.Errorf("%w: self-replace requires raw format without entrypoint or hooks", ErrInvalidRequest)
		}
	case InstallStrategyBundle:
		if spec.Format != InstallFormatTarGZ && spec.Format != InstallFormatZIP {
			return fmt.Errorf("%w: bundle requires tar.gz or zip format", ErrInvalidRequest)
		}
		if !safeInstallPath(spec.Entrypoint) {
			return fmt.Errorf("%w: bundle entrypoint must be a safe relative path", ErrInvalidRequest)
		}
		if len(spec.Hooks) > 3 {
			return fmt.Errorf("%w: install hook limit exceeded", ErrInvalidRequest)
		}
		phases := make(map[HookPhase]struct{}, len(spec.Hooks))
		for _, hook := range spec.Hooks {
			if err := validateInstallHook(hook); err != nil {
				return err
			}
			if _, duplicate := phases[hook.Phase]; duplicate {
				return fmt.Errorf("%w: duplicate install hook phase", ErrInvalidRequest)
			}
			phases[hook.Phase] = struct{}{}
		}
	default:
		return fmt.Errorf("%w: unsupported install strategy", ErrInvalidRequest)
	}
	return nil
}

func validateInstallHook(hook InstallHook) error {
	switch hook.Phase {
	case HookPhasePreflight, HookPhasePostInstall, HookPhaseVerify:
	default:
		return fmt.Errorf("%w: unsupported install hook phase", ErrInvalidRequest)
	}
	if !safeInstallPath(hook.Path) {
		return fmt.Errorf("%w: install hook path must be a safe relative path", ErrInvalidRequest)
	}
	if hook.TimeoutSeconds < 1 || hook.TimeoutSeconds > 300 {
		return fmt.Errorf("%w: install hook timeout must be between 1 and 300 seconds", ErrInvalidRequest)
	}
	if len(hook.Args) > 16 {
		return fmt.Errorf("%w: install hook argument limit exceeded", ErrInvalidRequest)
	}
	for _, argument := range hook.Args {
		if argument == "" || len(argument) > 1024 || strings.IndexByte(argument, 0) >= 0 {
			return fmt.Errorf("%w: invalid install hook argument", ErrInvalidRequest)
		}
	}
	return nil
}

func safeInstallPath(value string) bool {
	if value == "" || len(value) > 1024 || strings.Contains(value, "\\") || strings.IndexByte(value, 0) >= 0 {
		return false
	}
	cleaned := path.Clean(value)
	return cleaned == value && cleaned != "." && !strings.HasPrefix(cleaned, "/") &&
		cleaned != ".." && !strings.HasPrefix(cleaned, "../")
}

func encodeInstallSpec(spec *InstallSpec) ([]byte, error) {
	if spec == nil {
		return []byte("{}"), nil
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("encode install spec: %w", err)
	}
	return encoded, nil
}

func decodeInstallSpec(encoded []byte) (*InstallSpec, error) {
	if len(encoded) == 0 || string(encoded) == "{}" {
		return nil, nil
	}
	var spec InstallSpec
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return nil, fmt.Errorf("decode install spec: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return &spec, nil
}
