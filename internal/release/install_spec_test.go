package release

import (
	"errors"
	"reflect"
	"testing"
)

func TestInstallSpecValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		spec InstallSpec
		ok   bool
	}{
		{
			name: "self replace",
			spec: InstallSpec{Strategy: InstallStrategySelfReplace, Format: InstallFormatRaw, Mode: "0755"},
			ok:   true,
		},
		{
			name: "bundle",
			spec: InstallSpec{
				Strategy:   InstallStrategyBundle,
				Format:     InstallFormatTarGZ,
				Mode:       "0755",
				Entrypoint: "bin/edgectl",
				Hooks: []InstallHook{{
					Phase: HookPhaseVerify, Path: "bin/edgectl", Args: []string{"version"}, TimeoutSeconds: 15,
				}},
			},
			ok: true,
		},
		{
			name: "self replace hook",
			spec: InstallSpec{
				Strategy: InstallStrategySelfReplace,
				Format:   InstallFormatRaw,
				Mode:     "0755",
				Hooks:    []InstallHook{{Phase: HookPhaseVerify, Path: "cli", TimeoutSeconds: 5}},
			},
		},
		{
			name: "path traversal",
			spec: InstallSpec{
				Strategy: InstallStrategyBundle, Format: InstallFormatZIP, Mode: "0755", Entrypoint: "../cli",
			},
		},
		{
			name: "non executable",
			spec: InstallSpec{
				Strategy: InstallStrategyBundle, Format: InstallFormatZIP, Mode: "0644", Entrypoint: "cli",
			},
		},
		{
			name: "duplicate hooks",
			spec: InstallSpec{
				Strategy:   InstallStrategyBundle,
				Format:     InstallFormatZIP,
				Mode:       "0755",
				Entrypoint: "cli",
				Hooks: []InstallHook{
					{Phase: HookPhaseVerify, Path: "cli", TimeoutSeconds: 5},
					{Phase: HookPhaseVerify, Path: "cli", TimeoutSeconds: 5},
				},
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := test.spec.Validate()
			if test.ok && err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if !test.ok && !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("Validate() error = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

func TestInstallSpecRoundTripAndRejectsUnknownFields(t *testing.T) {
	t.Parallel()
	spec := &InstallSpec{Strategy: InstallStrategySelfReplace, Format: InstallFormatRaw, Mode: "0755"}
	encoded, err := encodeInstallSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeInstallSpec(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded == nil || !reflect.DeepEqual(decoded, spec) {
		t.Fatalf("decoded spec = %#v, want %#v", decoded, spec)
	}
	if _, err := decodeInstallSpec([]byte(`{"strategy":"self-replace","format":"raw","mode":"0755","extra":true}`)); err == nil {
		t.Fatal("decodeInstallSpec() accepted an unknown field")
	}
}
