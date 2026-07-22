package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
)

const usageText = `Usage:
  artifactctl [global flags] upload [flags] <local-file> <repository/artifact-path>
  artifactctl [global flags] download [flags] <repository/artifact-path>
  artifactctl [global flags] inspect <repository/artifact-path>
  artifactctl [global flags] pull [flags] <repository/package>

Global flags:
  --url URL              API base URL (or ARTIFACT_REPOSITORY_URL)
  --token-file PATH      Read the Bearer Token from PATH

Commands:
  upload                 Upload an immutable artifact with a computed SHA-256
  download               Download and verify an artifact by logical path
  inspect                Print artifact metadata as JSON
  pull                   Resolve, verify, and download a signed Channel artifact

Authentication:
  Set ARTIFACT_REPOSITORY_TOKEN or use --token-file. Tokens are deliberately not
  accepted as command-line values so they do not leak through shell history or ps.`

type LookupFunc func(string) (string, bool)

type usageError struct {
	message string
}

func (e usageError) Error() string { return e.message }

func Usage() string { return usageText }

func IsUsageError(err error) bool {
	var target usageError
	return errors.As(err, &target)
}

type globalOptions struct {
	rawURL    string
	tokenFile string
}

type application struct {
	stdout io.Writer
	stderr io.Writer
	lookup LookupFunc
}

func Run(ctx context.Context, args []string, stdout, stderr io.Writer, lookup LookupFunc) error {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if lookup == nil {
		lookup = os.LookupEnv
	}
	app := application{stdout: stdout, stderr: stderr, lookup: lookup}
	return app.run(ctx, args)
}

func (a application) run(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("artifactctl", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	options := globalOptions{}
	flags.StringVar(&options.rawURL, "url", "", "API base URL")
	flags.StringVar(&options.tokenFile, "token-file", "", "Bearer Token file")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprintln(a.stdout, Usage())
			return nil
		}
		return usageError{message: err.Error()}
	}
	remaining := flags.Args()
	if len(remaining) == 0 {
		return usageError{message: "a command is required"}
	}
	if remaining[0] == "help" {
		if len(remaining) != 1 {
			return usageError{message: "help does not accept arguments"}
		}
		_, _ = fmt.Fprintln(a.stdout, Usage())
		return nil
	}

	switch remaining[0] {
	case "upload":
		return a.runUpload(ctx, options, remaining[1:])
	case "download":
		return a.runDownload(ctx, options, remaining[1:])
	case "inspect":
		return a.runInspect(ctx, options, remaining[1:])
	case "pull":
		return a.runPull(ctx, options, remaining[1:])
	default:
		return usageError{message: fmt.Sprintf("unknown command %q", remaining[0])}
	}
}

func (a application) client(options globalOptions) (*client, error) {
	rawURL := strings.TrimSpace(options.rawURL)
	if rawURL == "" {
		rawURL, _ = a.lookup("ARTIFACT_REPOSITORY_URL")
		rawURL = strings.TrimSpace(rawURL)
	}
	if rawURL == "" {
		return nil, fmt.Errorf("API URL is required: use --url or ARTIFACT_REPOSITORY_URL")
	}

	var token string
	if strings.TrimSpace(options.tokenFile) != "" {
		encoded, err := os.ReadFile(strings.TrimSpace(options.tokenFile))
		if err != nil {
			return nil, fmt.Errorf("read token file: %w", err)
		}
		token = strings.TrimSpace(string(encoded))
	} else {
		token, _ = a.lookup("ARTIFACT_REPOSITORY_TOKEN")
		token = strings.TrimSpace(token)
	}
	if token == "" {
		return nil, fmt.Errorf("Bearer Token is required: use --token-file or ARTIFACT_REPOSITORY_TOKEN")
	}
	if strings.ContainsAny(token, " \t\r\n") {
		return nil, fmt.Errorf("Bearer Token must not contain whitespace")
	}
	return newClient(rawURL, token)
}

func (a application) runUpload(ctx context.Context, global globalOptions, args []string) error {
	flags := flag.NewFlagSet("upload", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	mediaType := flags.String("media-type", "application/octet-stream", "artifact media type")
	properties := flags.String("properties", "", "JSON object stored with the artifact")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprintln(a.stdout, "Usage: artifactctl [global flags] upload [--media-type TYPE] [--properties JSON] <local-file> <repository/artifact-path>")
			return nil
		}
		return usageError{message: "upload: " + err.Error()}
	}
	if flags.NArg() != 2 {
		return usageError{message: "upload requires <local-file> and <repository/artifact-path>"}
	}
	ref, err := parseArtifactReference(flags.Arg(1))
	if err != nil {
		return usageError{message: "upload: " + err.Error()}
	}
	encodedProperties, err := encodeProperties(*properties)
	if err != nil {
		return usageError{message: "upload: " + err.Error()}
	}
	apiClient, err := a.client(global)
	if err != nil {
		return err
	}
	artifact, err := apiClient.upload(ctx, flags.Arg(0), ref, strings.TrimSpace(*mediaType), encodedProperties)
	if err != nil {
		return err
	}
	return writeJSON(a.stdout, artifact)
}

func (a application) runDownload(ctx context.Context, global globalOptions, args []string) error {
	flags := flag.NewFlagSet("download", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	output := flags.String("output", "", "output file path, or - for stdout")
	flags.StringVar(output, "o", "", "output file path, or - for stdout")
	force := flags.Bool("force", false, "replace an existing output file")
	redirect := flags.Bool("redirect", false, "use a credential-free presigned download URL")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprintln(a.stdout, "Usage: artifactctl [global flags] download [-o PATH] [--force] [--redirect] <repository/artifact-path>")
			return nil
		}
		return usageError{message: "download: " + err.Error()}
	}
	if flags.NArg() != 1 {
		return usageError{message: "download requires <repository/artifact-path>"}
	}
	ref, err := parseArtifactReference(flags.Arg(0))
	if err != nil {
		return usageError{message: "download: " + err.Error()}
	}
	apiClient, err := a.client(global)
	if err != nil {
		return err
	}
	metadata, err := apiClient.metadata(ctx, ref)
	if err != nil {
		return err
	}
	response, err := apiClient.downloadArtifact(ctx, ref, *redirect)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	destination := *output
	if destination == "" {
		parts := strings.Split(ref.Path, "/")
		destination = parts[len(parts)-1]
	}
	return a.saveDownload(response.Body, destination, *force, expectedArtifact{
		SHA256: metadata.Sha256,
		Size:   metadata.Size,
	})
}

func (a application) runInspect(ctx context.Context, global globalOptions, args []string) error {
	flags := flag.NewFlagSet("inspect", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprintln(a.stdout, "Usage: artifactctl [global flags] inspect <repository/artifact-path>")
			return nil
		}
		return usageError{message: "inspect: " + err.Error()}
	}
	if flags.NArg() != 1 {
		return usageError{message: "inspect requires <repository/artifact-path>"}
	}
	ref, err := parseArtifactReference(flags.Arg(0))
	if err != nil {
		return usageError{message: "inspect: " + err.Error()}
	}
	apiClient, err := a.client(global)
	if err != nil {
		return err
	}
	metadata, err := apiClient.metadata(ctx, ref)
	if err != nil {
		return err
	}
	return writeJSON(a.stdout, metadata)
}

func (a application) runPull(ctx context.Context, global globalOptions, args []string) error {
	flags := flag.NewFlagSet("pull", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	channel := flags.String("channel", "stable", "candidate or stable")
	operatingSystem := flags.String("os", runtime.GOOS, "artifact operating system")
	architecture := flags.String("arch", runtime.GOARCH, "artifact architecture")
	variant := flags.String("variant", "", "artifact variant")
	role := flags.String("role", "binary", "artifact role")
	publicKey := flags.String("public-key", "", "trusted Ed25519 public key PEM")
	output := flags.String("output", "", "output file path, or - for stdout")
	flags.StringVar(output, "o", "", "output file path, or - for stdout")
	force := flags.Bool("force", false, "replace an existing output file")
	redirect := flags.Bool("redirect", false, "request a credential-free presigned download URL")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprintln(a.stdout, "Usage: artifactctl [global flags] pull --public-key PATH [--channel stable] [--os OS] [--arch ARCH] [-o PATH] <repository/package>")
			return nil
		}
		return usageError{message: "pull: " + err.Error()}
	}
	if flags.NArg() != 1 {
		return usageError{message: "pull requires <repository/package>"}
	}
	ref, err := parsePackageReference(flags.Arg(0))
	if err != nil {
		return usageError{message: "pull: " + err.Error()}
	}
	selection := channelSelection{
		Channel:  strings.TrimSpace(*channel),
		OS:       strings.TrimSpace(*operatingSystem),
		Arch:     strings.TrimSpace(*architecture),
		Variant:  strings.TrimSpace(*variant),
		Role:     strings.TrimSpace(*role),
		Redirect: *redirect,
	}
	if err := selection.validate(); err != nil {
		return usageError{message: "pull: " + err.Error()}
	}
	trustedKey := strings.TrimSpace(*publicKey)
	if trustedKey == "" {
		trustedKey, _ = a.lookup("ARTIFACT_REPOSITORY_PUBLIC_KEY")
		trustedKey = strings.TrimSpace(trustedKey)
	}
	if trustedKey == "" {
		return usageError{message: "pull requires --public-key or ARTIFACT_REPOSITORY_PUBLIC_KEY"}
	}
	apiClient, err := a.client(global)
	if err != nil {
		return err
	}
	resolved, err := apiClient.resolve(ctx, ref, selection)
	if err != nil {
		return err
	}
	if err := verifyResolution(trustedKey, ref, selection, resolved); err != nil {
		return fmt.Errorf("verify signed release: %w", err)
	}
	response, err := apiClient.downloadResolved(ctx, resolved.DownloadUrl)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	destination := *output
	if destination == "" {
		parts := strings.Split(resolved.Artifact.Path, "/")
		destination = parts[len(parts)-1]
	}
	return a.saveDownload(response.Body, destination, *force, expectedArtifact{
		SHA256: resolved.Artifact.Sha256,
		Size:   resolved.Artifact.Size,
	})
}

func writeJSON(destination io.Writer, value any) error {
	encoder := newJSONEncoder(destination)
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("write JSON output: %w", err)
	}
	return nil
}
