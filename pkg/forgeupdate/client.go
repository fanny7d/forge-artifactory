package forgeupdate

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"sync"
)

// ClientConfig wires one Forge source to signature verification and the two
// supported local installation strategies.
type ClientConfig struct {
	Source     *HTTPSource
	Verifier   Verifier
	SelfBinary SelfBinaryOptions
	Bundle     BundleOptions
}

// Client separates update discovery from installation. Check is read-only;
// the host CLI decides whether to prompt and calls Apply only after consent.
type Client struct {
	source     *HTTPSource
	verifier   Verifier
	selfBinary SelfBinaryOptions
	bundle     BundleOptions
}

type planState struct {
	mu   sync.Mutex
	used bool
}

// Plan is an immutable, one-shot update plan returned by Client.Check.
type Plan struct {
	owner      *Client
	resolution SourceResolution
	release    VerifiedRelease
	state      *planState
}

func (plan *Plan) Manifest() Manifest {
	if plan == nil {
		return Manifest{}
	}
	return plan.release.Manifest()
}

func (plan *Plan) Artifact() Artifact {
	if plan == nil {
		return Artifact{}
	}
	return plan.release.Artifact()
}

func (plan *Plan) Version() string {
	if plan == nil {
		return ""
	}
	return plan.release.manifest.Version
}

// ApplyResult contains exactly one receipt. The caller decides when to
// Finalize it or whether to Rollback.
type ApplyResult struct {
	kind       ArtifactKind
	selfBinary *SelfBinaryReceipt
	bundle     *BundleReceipt
}

func (result *ApplyResult) Kind() ArtifactKind {
	if result == nil {
		return ""
	}
	return result.kind
}

func (result *ApplyResult) SelfBinary() *SelfBinaryReceipt {
	if result == nil {
		return nil
	}
	return result.selfBinary
}

func (result *ApplyResult) Bundle() *BundleReceipt {
	if result == nil {
		return nil
	}
	return result.bundle
}

func NewClient(config ClientConfig) (*Client, error) {
	if config.Source == nil {
		return nil, fmt.Errorf("forgeupdate: Client Source is required")
	}
	if len(config.Verifier.TrustedKeys) == 0 {
		return nil, fmt.Errorf("forgeupdate: Client requires at least one trusted key")
	}
	return &Client{
		source:     config.Source,
		verifier:   cloneVerifier(config.Verifier),
		selfBinary: config.SelfBinary,
		bundle:     config.Bundle,
	}, nil
}

// Check resolves, verifies, and binds a newer signed release. It never opens
// the artifact URL and returns ErrNoUpdate when the stable release is not
// newer than Selection.CurrentVersion.
func (client *Client) Check(ctx context.Context, selection Selection) (*Plan, error) {
	if client == nil || client.source == nil {
		return nil, fmt.Errorf("%w: Client is nil", ErrInvalidPlan)
	}
	resolution, err := client.source.Resolve(ctx, selection)
	if err != nil {
		return nil, err
	}
	release, err := client.verifier.Verify(resolution.SignedManifest(), selection)
	if err != nil {
		return nil, err
	}
	if release.manifest.SchemaVersion != 2 {
		return nil, fmt.Errorf("%w: Forge HTTP updates require manifest v2", ErrInvalidManifest)
	}
	if err := bindResolution(resolution, release); err != nil {
		return nil, err
	}
	return &Plan{
		owner: client, resolution: resolution, release: release, state: new(planState),
	}, nil
}

// Apply consumes a Plan, opens its artifact, and dispatches to the signed
// self-replace or bundle strategy. Plans are one-shot; call Check again after
// any Apply attempt that needs to be retried.
func (client *Client) Apply(ctx context.Context, plan *Plan) (*ApplyResult, error) {
	if client == nil || plan == nil || plan.owner != client || plan.state == nil {
		return nil, ErrInvalidPlan
	}
	plan.state.mu.Lock()
	if plan.state.used {
		plan.state.mu.Unlock()
		return nil, ErrPlanUsed
	}
	plan.state.used = true
	plan.state.mu.Unlock()

	source, err := client.source.Open(ctx, plan.resolution, plan.release)
	if err != nil {
		return nil, err
	}
	defer func() { _ = source.Close() }()

	switch plan.release.artifact.Kind {
	case ArtifactBinary:
		receipt, err := InstallSelfBinary(ctx, source, plan.release, client.selfBinary)
		if err != nil {
			return nil, err
		}
		return &ApplyResult{kind: ArtifactBinary, selfBinary: receipt}, nil
	case ArtifactBundle:
		receipt, err := InstallBundle(ctx, source, plan.release, client.bundle)
		if err != nil {
			return nil, err
		}
		return &ApplyResult{kind: ArtifactBundle, bundle: receipt}, nil
	default:
		return nil, fmt.Errorf("%w: unsupported signed artifact kind %q", ErrInvalidPlan, plan.release.artifact.Kind)
	}
}

func cloneVerifier(source Verifier) Verifier {
	clone := Verifier{
		TrustedKeys:      make(map[string]ed25519.PublicKey, len(source.TrustedKeys)),
		MaxManifestBytes: source.MaxManifestBytes,
	}
	for keyID, key := range source.TrustedKeys {
		clone.TrustedKeys[keyID] = append(ed25519.PublicKey(nil), key...)
	}
	return clone
}
