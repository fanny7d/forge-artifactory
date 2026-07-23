package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	artifactdomain "superfan.myasustor.com/fanchao/artifact-repository/internal/artifact"
	identity "superfan.myasustor.com/fanchao/artifact-repository/internal/auth"
	channeldomain "superfan.myasustor.com/fanchao/artifact-repository/internal/channel"
	productdomain "superfan.myasustor.com/fanchao/artifact-repository/internal/product"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/ratelimit"
	releasedomain "superfan.myasustor.com/fanchao/artifact-repository/internal/release"
)

type InstallProductService interface {
	GetByInstallKey(context.Context, uuid.UUID) (productdomain.Product, error)
}

type InstallReleaseService interface {
	Get(context.Context, identity.Actor, string, string, string) (releasedomain.Release, error)
}

type installHandlers struct {
	products  InstallProductService
	channels  ChannelService
	releases  InstallReleaseService
	artifacts ArtifactService
}

var (
	installHostPattern   = regexp.MustCompile(`^[A-Za-z0-9.:[\]-]+$`)
	installSHA256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// installGlobalRateLimitKey is a reserved, stable bucket that gates all
	// public install requests before any attacker-controlled UUID is acquired.
	installGlobalRateLimitKey = uuid.MustParse("00000000-0000-4000-8000-000000000001")
)

func registerInstallRoutes(
	router chi.Router,
	products InstallProductService,
	channels ChannelService,
	releases InstallReleaseService,
	artifacts ArtifactService,
	limiter RequestLimiter,
) {
	if products == nil || channels == nil || artifacts == nil {
		return
	}
	handlers := installHandlers{products: products, channels: channels, releases: releases, artifacts: artifacts}
	base := "/i/{installKey}/{product}"
	register := func(pattern string, handler http.HandlerFunc) {
		if limiter == nil {
			router.Get(pattern, handler)
			return
		}
		router.With(installRateLimitMiddleware(limiter)).Get(pattern, handler)
	}
	register(base+"/install", handlers.script)
	register(base+"/resolve", handlers.resolve)
	register(base+"/download", handlers.download)
}

func installRateLimitMiddleware(limiter RequestLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			globalDecision := limiter.Acquire(installGlobalRateLimitKey, ratelimit.ClassRead)
			if !globalDecision.Allowed {
				w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(globalDecision.RetryAfter)))
				writeRequestProblem(w, r, Problem{
					Type:   "about:blank",
					Title:  "Too Many Requests",
					Status: http.StatusTooManyRequests,
					Code:   "rate-limit-exceeded",
				})
				return
			}
			if globalDecision.Release != nil {
				defer globalDecision.Release()
			}
			key, err := uuid.Parse(chi.URLParam(r, "installKey"))
			if err != nil || key == installGlobalRateLimitKey {
				next.ServeHTTP(w, r)
				return
			}
			decision := limiter.Acquire(key, ratelimit.ClassRead)
			if !decision.Allowed {
				w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(decision.RetryAfter)))
				writeRequestProblem(w, r, Problem{
					Type:   "about:blank",
					Title:  "Too Many Requests",
					Status: http.StatusTooManyRequests,
					Code:   "rate-limit-exceeded",
				})
				return
			}
			if decision.Release != nil {
				defer decision.Release()
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (h installHandlers) script(w http.ResponseWriter, r *http.Request) {
	product, requestKey, ok := h.product(w, r)
	if !ok {
		return
	}
	baseURL, err := installBaseURL(r)
	if err != nil {
		writeHandlerError(w, r, productdomain.ErrInvalidRequest)
		return
	}
	installURL := fmt.Sprintf(
		"%s/i/%s/%s/install",
		baseURL,
		requestKey.String(),
		product.Slug,
	)
	osValue := r.URL.Query().Get("os")
	arch := r.URL.Query().Get("arch")
	if osValue == "" && arch == "" && r.URL.Query().Get("variant") == "" {
		writeInstallScript(w, renderInstallDispatcher(product, installURL))
		return
	}
	resolved, osValue, arch, variant, ok := h.resolveProduct(w, r, product)
	if !ok {
		return
	}
	install, err := selectedInstallPlan(resolved.Manifest, osValue, arch, variant)
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	query := url.Values{
		"os":      {osValue},
		"arch":    {arch},
		"version": {resolved.Version},
		"sha256":  {resolved.Artifact.SHA256},
	}
	if variant != "" {
		query.Set("variant", variant)
	}
	downloadURL := fmt.Sprintf(
		"%s/i/%s/%s/download?%s",
		baseURL,
		requestKey.String(),
		product.Slug,
		query.Encode(),
	)
	switch install.Strategy {
	case releasedomain.InstallStrategySelfReplace:
		writeInstallScript(w, renderRawInstallScript(product, resolved.Version, resolved.Artifact.SHA256, install, downloadURL))
	case releasedomain.InstallStrategyBundle:
		writeInstallScript(w, renderBundleInstallScript(product, resolved.Version, resolved.Artifact.SHA256, install, downloadURL))
	default:
		writeHandlerError(w, r, fmt.Errorf("%w: unsupported install strategy", productdomain.ErrConflict))
	}
}

func writeInstallScript(w http.ResponseWriter, script string) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	_, _ = w.Write([]byte(script))
}

func renderInstallDispatcher(product productdomain.Product, installURL string) string {
	return replaceInstallScriptValues(`#!/bin/sh
set -eu

product=@@PRODUCT@@
install_url=@@INSTALL_URL@@

case "$(uname -s)" in
  Linux) forge_os=linux ;;
  Darwin) forge_os=darwin ;;
  *) echo "Unsupported operating system: $(uname -s)" >&2; exit 1 ;;
esac

case "$(uname -m)" in
  x86_64|amd64) forge_arch=amd64 ;;
  arm64|aarch64) forge_arch=arm64 ;;
  *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

script_file=$(mktemp "${TMPDIR:-/tmp}/forge-install-script.XXXXXX")
cleanup() {
  rm -f "$script_file"
}
trap cleanup EXIT HUP INT TERM
curl -fsSL --retry 2 \
  "${install_url}?os=${forge_os}&arch=${forge_arch}" \
  -o "$script_file"
/bin/sh "$script_file"
trap - EXIT HUP INT TERM
rm -f "$script_file"
`, map[string]string{
		"PRODUCT":     shellSingleQuote(product.Slug),
		"INSTALL_URL": shellSingleQuote(installURL),
	})
}

func renderRawInstallScript(
	product productdomain.Product,
	version, sha256 string,
	install releasedomain.InstallSpec,
	downloadURL string,
) string {
	return replaceInstallScriptValues(`#!/bin/sh
set -eu

product=@@PRODUCT@@
command_name=@@COMMAND@@
version=@@VERSION@@
expected=@@SHA256@@
download_url=@@DOWNLOAD_URL@@
install_dir=${FORGE_INSTALL_DIR:-"$HOME/.local/bin"}

mkdir -p "$install_dir"
candidate=$(mktemp "$install_dir/.${command_name}.new.XXXXXX")
cleanup() {
  rm -f "$candidate"
}
trap cleanup EXIT HUP INT TERM

curl -fsSL --retry 2 "$download_url" -o "$candidate"
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$candidate" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  actual=$(shasum -a 256 "$candidate" | awk '{print $1}')
else
  echo "${product}: sha256sum or shasum is required" >&2
  exit 1
fi
if [ "$actual" != "$expected" ]; then
  echo "${product}: checksum verification failed" >&2
  exit 1
fi

chmod @@MODE@@ "$candidate"
target="$install_dir/$command_name"
if [ -e "$target" ] || [ -L "$target" ]; then
  cp -p "$target" "$target.old"
fi
mv -f "$candidate" "$target"
trap - EXIT HUP INT TERM

echo "Installed ${product} ${version} to ${target}"
case ":$PATH:" in
  *":$install_dir:"*) ;;
  *) echo "Add $install_dir to PATH to run ${command_name}." ;;
esac
`, map[string]string{
		"PRODUCT":      shellSingleQuote(product.Slug),
		"COMMAND":      shellSingleQuote(product.CommandName),
		"VERSION":      shellSingleQuote(version),
		"SHA256":       shellSingleQuote(sha256),
		"DOWNLOAD_URL": shellSingleQuote(downloadURL),
		"MODE":         shellSingleQuote(install.Mode),
	})
}

func renderBundleInstallScript(
	product productdomain.Product,
	version, sha256 string,
	install releasedomain.InstallSpec,
	downloadURL string,
) string {
	return replaceInstallScriptValues(`#!/bin/sh
set -eu

product=@@PRODUCT@@
command_name=@@COMMAND@@
version=@@VERSION@@
expected=@@SHA256@@
download_url=@@DOWNLOAD_URL@@
archive_format=@@FORMAT@@
entrypoint=@@ENTRYPOINT@@
entrypoint_mode=@@MODE@@
root=${FORGE_INSTALL_ROOT:-"$HOME/.local/share/forge/$product"}
install_dir=${FORGE_INSTALL_DIR:-"$HOME/.local/bin"}
versions="$root/versions"
target="$versions/$version"
current="$root/current"
launcher="$install_dir/$command_name"

mkdir -p "$versions" "$install_dir"
if [ -e "$target" ] || [ -L "$target" ]; then
  echo "${product}: version ${version} is already installed at ${target}" >&2
  exit 1
fi
if [ -e "$current" ] && [ ! -L "$current" ]; then
  echo "${product}: ${current} exists and is not a symlink" >&2
  exit 1
fi
if [ -d "$launcher" ] && [ ! -L "$launcher" ]; then
  echo "${product}: ${launcher} exists and is a directory" >&2
  exit 1
fi

archive=$(mktemp "$versions/.${version}.archive.XXXXXX")
staging=$(mktemp -d "$versions/.${version}.staging.XXXXXX")
names=$(mktemp "$versions/.${version}.names.XXXXXX")
metadata=$(mktemp "$versions/.${version}.metadata.XXXXXX")
current_tmp="$root/.current.new.$$"
launcher_tmp="$install_dir/.${command_name}.new.$$"
launcher_backup=
success=0
committed=0
current_changed=0
launcher_saved=0
launcher_changed=0
had_current=0
old_current=

if [ -L "$current" ]; then
  had_current=1
  old_current=$(readlink "$current")
  case "$old_current" in
    versions/*) old_version=${old_current#versions/} ;;
    *)
      echo "${product}: existing current symlink is not a canonical versions/<semver> target" >&2
      exit 1
      ;;
  esac
  case "$old_version" in
    */*|"")
      echo "${product}: existing current symlink is not a canonical versions/<semver> target" >&2
      exit 1
      ;;
  esac
  if ! printf '%s\n' "$old_version" | LC_ALL=C awk '
    BEGIN {
      numeric = "(0|[1-9][0-9]*)"
      identifier = "(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)"
      identifiers = identifier "([.]" identifier ")*"
      build = "[0-9A-Za-z-]+([.][0-9A-Za-z-]+)*"
      semver = "^" numeric "[.]" numeric "[.]" numeric "(-" identifiers ")?([+]" build ")?$"
      bad = 0
    }
    NR != 1 || $0 !~ semver { bad = 1 }
    END { exit bad ? 1 : 0 }
  '; then
    echo "${product}: existing current symlink is not a canonical versions/<semver> target" >&2
    exit 1
  fi
fi

atomic_replace_symlink() {
  source_link=$1
  destination_link=$2
  if mv -h -f "$source_link" "$destination_link" 2>/dev/null; then
    return 0
  fi
  if mv -T -f "$source_link" "$destination_link" 2>/dev/null; then
    return 0
  fi
  echo "${product}: mv cannot atomically replace symlinks on this system" >&2
  return 1
}

cleanup() {
  exit_code=$?
  trap - EXIT HUP INT TERM
  set +e
  if [ "$success" -ne 1 ]; then
    current_restored=1
    launcher_restored=1
    if [ "$launcher_saved" -eq 1 ]; then
      rm -f "$launcher" || launcher_restored=0
      mv -f "$launcher_backup" "$launcher" || launcher_restored=0
    elif [ "$launcher_changed" -eq 1 ]; then
      rm -f "$launcher" || launcher_restored=0
    fi
    if [ "$current_changed" -eq 1 ]; then
      if [ "$had_current" -eq 1 ]; then
        rm -f "$current_tmp"
        ln -s "$old_current" "$current_tmp"
        atomic_replace_symlink "$current_tmp" "$current" || current_restored=0
      else
        rm -f "$current" || current_restored=0
      fi
    fi
    if [ "$committed" -eq 1 ] && [ "$current_restored" -eq 1 ] && [ "$launcher_restored" -eq 1 ]; then
      rm -rf "$target"
    fi
  elif [ "$launcher_saved" -eq 1 ]; then
    rm -f "$launcher_backup"
  fi
  rm -f "$archive" "$names" "$metadata" "$current_tmp" "$launcher_tmp"
  if [ -n "$staging" ]; then
    rm -rf "$staging"
  fi
  exit "$exit_code"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

curl -fsSL --retry 2 "$download_url" -o "$archive"
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$archive" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  actual=$(shasum -a 256 "$archive" | awk '{print $1}')
else
  echo "${product}: sha256sum or shasum is required" >&2
  exit 1
fi
if [ "$actual" != "$expected" ]; then
  echo "${product}: checksum verification failed" >&2
  exit 1
fi

validate_archive_names() {
  LC_ALL=C awk '
    BEGIN { bad = 0 }
    {
      name = $0
      if (name == "" || substr(name, 1, 1) == "/" || index(name, "\\") != 0 ||
          name ~ /(^|\/)\.\.(\/|$)/ || name ~ /[[:cntrl:]]/) {
        bad = 1
      }
    }
    END { exit bad ? 1 : 0 }
  ' "$1"
}

case "$archive_format" in
  tar.gz)
    command -v tar >/dev/null 2>&1 || {
      echo "${product}: tar is required to install this bundle" >&2
      exit 1
    }
    tar -tzf "$archive" >"$names"
    tar -tzvf "$archive" >"$metadata"
    validate_archive_names "$names" || {
      echo "${product}: archive contains an unsafe path" >&2
      exit 1
    }
    listed_count=$(wc -l <"$names" | tr -d ' ')
    metadata_count=$(LC_ALL=C awk '
      /^[bcdhlps-]/ {
        count++
        if (substr($0, 1, 1) != "-" && substr($0, 1, 1) != "d") bad = 1
      }
      END {
        if (bad) exit 2
        print count + 0
      }
    ' "$metadata") || {
      echo "${product}: archive contains a link or special file" >&2
      exit 1
    }
    [ "$listed_count" = "$metadata_count" ] || {
      echo "${product}: archive metadata is ambiguous or unsafe" >&2
      exit 1
    }
    (umask 022 && tar -xzf "$archive" -C "$staging")
    ;;
  zip)
    command -v unzip >/dev/null 2>&1 || {
      echo "${product}: unzip is required to install this bundle" >&2
      exit 1
    }
    unzip -Z -1 "$archive" >"$names"
    unzip -Z -l "$archive" >"$metadata"
    validate_archive_names "$names" || {
      echo "${product}: archive contains an unsafe path" >&2
      exit 1
    }
    listed_count=$(wc -l <"$names" | tr -d ' ')
    metadata_count=$(LC_ALL=C awk '
      /^[-bcdhlps]/ {
        count++
        if (substr($0, 1, 1) != "-" && substr($0, 1, 1) != "d") bad = 1
      }
      END {
        if (bad) exit 2
        print count + 0
      }
    ' "$metadata") || {
      echo "${product}: archive contains a link or special file" >&2
      exit 1
    }
    [ "$listed_count" = "$metadata_count" ] || {
      echo "${product}: archive metadata is ambiguous or unsafe" >&2
      exit 1
    }
    (umask 022 && unzip -q "$archive" -d "$staging")
    ;;
  *)
    echo "${product}: unsupported bundle format ${archive_format}" >&2
    exit 1
    ;;
esac

staged_entrypoint="$staging/$entrypoint"
validate_staging() {
  unsafe_entry=$(find "$staging" ! -type f ! -type d -print | sed -n '1p')
  if [ -n "$unsafe_entry" ]; then
    echo "${product}: extracted bundle contains a link or special file" >&2
    return 1
  fi
  if [ ! -f "$staged_entrypoint" ] || [ -L "$staged_entrypoint" ]; then
    echo "${product}: bundle entrypoint ${entrypoint} is missing or unsafe" >&2
    return 1
  fi
  find "$staging" -exec chmod u-s,g-s {} \;
  chmod "$entrypoint_mode" "$staged_entrypoint"
}
validate_staging

run_hook() {
  hook_phase=$1
  hook_timeout=$2
  hook_root=$3
  hook_executable=$4
  shift 4
  if [ ! -f "$hook_executable" ] || [ -L "$hook_executable" ] || [ ! -x "$hook_executable" ]; then
    echo "${product}: ${hook_phase} hook is missing, unsafe, or not executable" >&2
    return 1
  fi
  if command -v timeout >/dev/null 2>&1; then
    (
      cd "$hook_root"
      FORGEUPDATE_HOOK_PHASE="$hook_phase" FORGEUPDATE_VERSION="$version" \
        timeout "${hook_timeout}s" "$hook_executable" "$@"
    )
  elif command -v gtimeout >/dev/null 2>&1; then
    (
      cd "$hook_root"
      FORGEUPDATE_HOOK_PHASE="$hook_phase" FORGEUPDATE_VERSION="$version" \
        gtimeout "${hook_timeout}s" "$hook_executable" "$@"
    )
  else
    # POSIX sh has no portable timeout primitive. The signed timeout is
    # enforced when timeout(1) or gtimeout(1) is available.
    echo "${product}: timeout utility unavailable; ${hook_phase} hook timeout cannot be enforced" >&2
    (
      cd "$hook_root"
      FORGEUPDATE_HOOK_PHASE="$hook_phase" FORGEUPDATE_VERSION="$version" \
        "$hook_executable" "$@"
    )
  fi
}

@@PREFLIGHT_HOOKS@@
validate_staging

mv "$staging" "$target"
staging=
committed=1

rm -f "$current_tmp"
ln -s "versions/$version" "$current_tmp"
atomic_replace_symlink "$current_tmp" "$current"
current_changed=1

if [ -e "$launcher" ] || [ -L "$launcher" ]; then
  launcher_backup=$(mktemp "$install_dir/.${command_name}.old.XXXXXX")
  rm -f "$launcher_backup"
  mv "$launcher" "$launcher_backup"
  launcher_saved=1
fi
rm -f "$launcher_tmp"
ln -s "$root/current/$entrypoint" "$launcher_tmp"
atomic_replace_symlink "$launcher_tmp" "$launcher"
launcher_changed=1

@@POST_INSTALL_HOOKS@@
@@VERIFY_HOOKS@@

success=1
echo "Installed ${product} ${version} to ${target}"
case ":$PATH:" in
  *":$install_dir:"*) ;;
  *) echo "Add $install_dir to PATH to run ${command_name}." ;;
esac
`, map[string]string{
		"PRODUCT":            shellSingleQuote(product.Slug),
		"COMMAND":            shellSingleQuote(product.CommandName),
		"VERSION":            shellSingleQuote(version),
		"SHA256":             shellSingleQuote(sha256),
		"DOWNLOAD_URL":       shellSingleQuote(downloadURL),
		"FORMAT":             shellSingleQuote(string(install.Format)),
		"ENTRYPOINT":         shellSingleQuote(install.Entrypoint),
		"MODE":               shellSingleQuote(install.Mode),
		"PREFLIGHT_HOOKS":    renderBundleHookCalls(install.Hooks, releasedomain.HookPhasePreflight, "staging"),
		"POST_INSTALL_HOOKS": renderBundleHookCalls(install.Hooks, releasedomain.HookPhasePostInstall, "target"),
		"VERIFY_HOOKS":       renderBundleHookCalls(install.Hooks, releasedomain.HookPhaseVerify, "target"),
	})
}

func renderBundleHookCalls(hooks []releasedomain.InstallHook, phase releasedomain.HookPhase, rootVariable string) string {
	var rendered strings.Builder
	for _, hook := range hooks {
		if hook.Phase != phase {
			continue
		}
		fmt.Fprintf(
			&rendered,
			"run_hook %s %d \"$%s\" \"$%s\"/%s",
			shellSingleQuote(string(hook.Phase)),
			hook.TimeoutSeconds,
			rootVariable,
			rootVariable,
			shellSingleQuote(hook.Path),
		)
		for _, argument := range hook.Args {
			rendered.WriteByte(' ')
			rendered.WriteString(shellSingleQuote(argument))
		}
		rendered.WriteByte('\n')
	}
	if rendered.Len() == 0 {
		return ":"
	}
	return strings.TrimSuffix(rendered.String(), "\n")
}

func replaceInstallScriptValues(script string, values map[string]string) string {
	arguments := make([]string, 0, len(values)*2)
	for name, value := range values {
		arguments = append(arguments, "@@"+name+"@@", value)
	}
	return strings.NewReplacer(arguments...).Replace(script)
}

func (h installHandlers) resolve(w http.ResponseWriter, r *http.Request) {
	product, key, ok := h.product(w, r)
	if !ok {
		return
	}
	resolved, osValue, arch, variant, ok := h.resolveProduct(w, r, product)
	if !ok {
		return
	}
	query := url.Values{
		"os":      {osValue},
		"arch":    {arch},
		"version": {resolved.Version},
		"sha256":  {resolved.Artifact.SHA256},
	}
	if variant != "" {
		query.Set("variant", variant)
	}
	downloadURL := fmt.Sprintf(
		"/i/%s/%s/download?%s",
		key.String(),
		product.Slug,
		query.Encode(),
	)
	writeJSON(w, http.StatusOK, ResolveResponse{
		Version:   resolved.Version,
		Manifest:  base64.RawURLEncoding.EncodeToString(resolved.Manifest),
		KeyId:     resolved.KeyID,
		Signature: base64.RawURLEncoding.EncodeToString(resolved.Signature),
		Artifact: ResolveArtifact{
			Path: resolved.Artifact.Path, Os: resolved.Artifact.OS, Arch: resolved.Artifact.Arch,
			Variant: resolved.Artifact.Variant, Role: resolved.Artifact.Role,
			Sha256: resolved.Artifact.SHA256, Size: resolved.Artifact.Size,
		},
		DownloadUrl: downloadURL,
	})
}

func (h installHandlers) download(w http.ResponseWriter, r *http.Request) {
	product, _, ok := h.product(w, r)
	if !ok {
		return
	}
	selection, ok := h.downloadSelection(w, r, product)
	if !ok {
		return
	}
	redirect := false
	result, err := h.artifacts.Open(r.Context(), artifactdomain.OpenRequest{
		Actor: identity.Actor{
			Scopes: identity.NewScopeSet(identity.ScopeAdmin),
		},
		RepositoryKey: product.RepositoryKey,
		RawPath:       selection.path,
		Redirect:      &redirect,
	})
	if err != nil {
		writeHandlerError(w, r, err)
		return
	}
	if result.Object.Body == nil || result.Object.Seeker == nil {
		if result.Object.Body != nil {
			_ = result.Object.Body.Close()
		}
		writeHandlerError(w, r, fmt.Errorf("install Artifact is not seekable"))
		return
	}
	defer func() { _ = result.Object.Body.Close() }()
	w.Header().Set("Content-Type", result.Metadata.MediaType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(result.Metadata.Filename, `"`, "")+`"`)
	w.Header().Set("X-Checksum-Sha256", selection.sha256)
	w.Header().Set("X-Forge-Version", selection.version)
	w.Header().Set("X-Forge-Install-Strategy", string(selection.install.Strategy))
	w.Header().Set("X-Forge-Install-Format", string(selection.install.Format))
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, result.Metadata.Filename, result.Metadata.CreatedAt, result.Object.Seeker)
}

type installDownloadSelection struct {
	version string
	path    string
	sha256  string
	install releasedomain.InstallSpec
}

func (h installHandlers) downloadSelection(
	w http.ResponseWriter,
	r *http.Request,
	product productdomain.Product,
) (installDownloadSelection, bool) {
	osValue := r.URL.Query().Get("os")
	arch := r.URL.Query().Get("arch")
	variant := r.URL.Query().Get("variant")
	if !validCoordinate.MatchString(osValue) || !validCoordinate.MatchString(arch) ||
		!validOptionalCoordinate.MatchString(variant) {
		writeHandlerError(w, r, channeldomain.ErrInvalidRequest)
		return installDownloadSelection{}, false
	}
	version := r.URL.Query().Get("version")
	if version == "" {
		resolved, _, _, _, ok := h.resolveProduct(w, r, product)
		if !ok {
			return installDownloadSelection{}, false
		}
		install, err := selectedInstallPlan(resolved.Manifest, osValue, arch, variant)
		if err != nil {
			writeHandlerError(w, r, err)
			return installDownloadSelection{}, false
		}
		return installDownloadSelection{
			version: resolved.Version,
			path:    resolved.Artifact.Path,
			sha256:  resolved.Artifact.SHA256,
			install: install,
		}, true
	}
	if !releasedomain.ValidVersion(version) {
		writeHandlerError(w, r, releasedomain.ErrInvalidRequest)
		return installDownloadSelection{}, false
	}
	expectedSHA256 := r.URL.Query().Get("sha256")
	if !installSHA256Pattern.MatchString(expectedSHA256) {
		writeHandlerError(w, r, releasedomain.ErrInvalidRequest)
		return installDownloadSelection{}, false
	}
	if h.releases == nil {
		writeHandlerError(w, r, fmt.Errorf("version-pinned install downloads are unavailable"))
		return installDownloadSelection{}, false
	}
	release, err := h.releases.Get(
		r.Context(),
		installActor(),
		product.RepositoryKey,
		product.PackageName,
		version,
	)
	if err != nil {
		writeHandlerError(w, r, err)
		return installDownloadSelection{}, false
	}
	if release.State != "published" {
		writeHandlerError(w, r, releasedomain.ErrNotFound)
		return installDownloadSelection{}, false
	}
	for _, artifact := range release.Artifacts {
		if artifact.OS != osValue || artifact.Arch != arch || artifact.Variant != variant || artifact.Role != "binary" {
			continue
		}
		if artifact.Install == nil {
			writeHandlerError(w, r, fmt.Errorf("%w: selected Artifact has no install plan", productdomain.ErrConflict))
			return installDownloadSelection{}, false
		}
		if artifact.Artifact.SHA256 != expectedSHA256 {
			writeHandlerError(w, r, releasedomain.ErrNotFound)
			return installDownloadSelection{}, false
		}
		return installDownloadSelection{
			version: release.Version,
			path:    artifact.Artifact.Path,
			sha256:  artifact.Artifact.SHA256,
			install: *artifact.Install,
		}, true
	}
	writeHandlerError(w, r, releasedomain.ErrNotFound)
	return installDownloadSelection{}, false
}

func (h installHandlers) product(w http.ResponseWriter, r *http.Request) (productdomain.Product, uuid.UUID, bool) {
	key, err := uuid.Parse(chi.URLParam(r, "installKey"))
	if err != nil {
		writeHandlerError(w, r, productdomain.ErrNotFound)
		return productdomain.Product{}, uuid.Nil, false
	}
	product, err := h.products.GetByInstallKey(r.Context(), key)
	if errors.Is(err, productdomain.ErrNotFound) || errors.Is(err, productdomain.ErrInvalidRequest) ||
		(err == nil && product.Slug != chi.URLParam(r, "product")) {
		writeHandlerError(w, r, productdomain.ErrNotFound)
		return productdomain.Product{}, uuid.Nil, false
	}
	if err != nil {
		writeHandlerError(w, r, err)
		return productdomain.Product{}, uuid.Nil, false
	}
	return product, key, true
}

func (h installHandlers) resolveProduct(
	w http.ResponseWriter,
	r *http.Request,
	product productdomain.Product,
) (channeldomain.Resolution, string, string, string, bool) {
	osValue := r.URL.Query().Get("os")
	arch := r.URL.Query().Get("arch")
	variant := r.URL.Query().Get("variant")
	if !validCoordinate.MatchString(osValue) || !validCoordinate.MatchString(arch) ||
		!validOptionalCoordinate.MatchString(variant) {
		writeHandlerError(w, r, channeldomain.ErrInvalidRequest)
		return channeldomain.Resolution{}, "", "", "", false
	}
	redirect := false
	resolved, err := h.channels.Resolve(r.Context(), channeldomain.ResolveRequest{
		Actor:         installActor(),
		RepositoryKey: product.RepositoryKey,
		PackageName:   product.PackageName,
		ChannelName:   "stable",
		OS:            osValue,
		Arch:          arch,
		Variant:       variant,
		Role:          "binary",
		Redirect:      &redirect,
	})
	if err != nil {
		writeHandlerError(w, r, err)
		return channeldomain.Resolution{}, "", "", "", false
	}
	return resolved, osValue, arch, variant, true
}

func selectedInstallPlan(
	manifest []byte,
	osValue, arch, variant string,
) (releasedomain.InstallSpec, error) {
	var document struct {
		SchemaVersion int `json:"schemaVersion"`
		Artifacts     []struct {
			OS      string                     `json:"os"`
			Arch    string                     `json:"arch"`
			Variant string                     `json:"variant"`
			Role    string                     `json:"role"`
			Install *releasedomain.InstallSpec `json:"install"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(manifest, &document); err != nil || document.SchemaVersion != 2 {
		return releasedomain.InstallSpec{}, fmt.Errorf(
			"%w: stable release does not contain a valid install plan",
			productdomain.ErrConflict,
		)
	}
	for _, artifact := range document.Artifacts {
		if artifact.OS == osValue && artifact.Arch == arch && artifact.Variant == variant && artifact.Role == "binary" {
			if artifact.Install == nil {
				break
			}
			if err := artifact.Install.Validate(); err != nil {
				return releasedomain.InstallSpec{}, fmt.Errorf(
					"%w: selected Artifact has an invalid install plan",
					productdomain.ErrConflict,
				)
			}
			return *artifact.Install, nil
		}
	}
	return releasedomain.InstallSpec{}, fmt.Errorf("%w: selected Artifact has no install plan", productdomain.ErrConflict)
}

func installActor() identity.Actor {
	return identity.Actor{Scopes: identity.NewScopeSet(identity.ScopeAdmin)}
}

func installBaseURL(r *http.Request) (string, error) {
	if !installHostPattern.MatchString(r.Host) {
		return "", fmt.Errorf("invalid request host")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); forwarded != "" {
		if forwarded != "http" && forwarded != "https" {
			return "", fmt.Errorf("invalid forwarded scheme")
		}
		scheme = forwarded
	}
	return scheme + "://" + r.Host, nil
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}
