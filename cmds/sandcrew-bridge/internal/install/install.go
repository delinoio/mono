// SPDX-License-Identifier: Apache-2.0

// Package install finds or installs the pinned agent components of a vendor:
// the runtime (Node.js) from its official archive, verified by SHA-256
// before extraction, and the ACP adapter with `npm ci --ignore-scripts` and
// the embedded package-lock.json, which verifies the integrity of every
// package. The owning contract is
// docs/cmds-sandcrew-bridge-foundation.md#pinned-agent-installation.
package install

import (
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/delinoio/mono/cmds/sandcrew-bridge/internal/pins"
	"github.com/delinoio/mono/internal/backoff"
	"github.com/delinoio/mono/internal/fsutil"
	sandcrewv1 "github.com/delinoio/mono/protos/delino/sandcrew/v1"
)

// Attempts is the number of download attempts of one component.
const Attempts = 3

// DownloadBudget limits the downloads of one start.
const DownloadBudget = 10 * time.Minute

// versionTimeout limits a `--version` check of a preinstalled runtime.
const versionTimeout = 10 * time.Second

// markerName is the file that marks a complete installation directory. It
// holds the SHA-256 of the archive or the lockfile that the directory was
// installed from.
const markerName = ".sandcrew-installed"

// Error is an installation failure with the bridge error kind that it
// produces: AGENT_INSTALL_FAILED or CHECKSUM_MISMATCH.
type Error struct {
	Kind sandcrewv1.BridgeErrorKind
	Err  error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

func installFailed(format string, args ...any) *Error {
	return &Error{Kind: sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_AGENT_INSTALL_FAILED, Err: fmt.Errorf(format, args...)}
}

func checksumMismatch(format string, args ...any) *Error {
	return &Error{Kind: sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_CHECKSUM_MISMATCH, Err: fmt.Errorf(format, args...)}
}

// Runner runs a command and returns its combined output.
type Runner interface {
	Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error)
}

// ExecRunner runs commands with os/exec.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	return cmd.CombinedOutput()
}

// Installer installs the components of one vendor into AgentsDir.
type Installer struct {
	// AgentsDir is `<state>/agents`.
	AgentsDir string
	Target    pins.Target
	Pins      *pins.Pins
	HTTP      *http.Client
	Runner    Runner
	// LookPath finds a preinstalled runtime. Defaults to exec.LookPath.
	LookPath func(string) (string, error)
	// Environ is the base environment of npm. Defaults to os.Environ.
	Environ func() []string
	Logger  *slog.Logger
	// BackoffMin and BackoffMax bound the delay between attempts.
	// Default 2s and 30s.
	BackoffMin, BackoffMax time.Duration
	// Budget limits the downloads. Defaults to DownloadBudget.
	Budget time.Duration
}

// Result describes the installed agent.
type Result struct {
	// RuntimePath is the runtime executable (node).
	RuntimePath string
	// AdapterEntry is the script that starts the ACP adapter.
	AdapterEntry string
	// RuntimePreinstalled and AdapterPreinstalled report whether the image
	// already contained the pinned version.
	RuntimePreinstalled bool
	AdapterPreinstalled bool
}

func (i *Installer) defaults() {
	if i.HTTP == nil {
		i.HTTP = http.DefaultClient
	}
	if i.Runner == nil {
		i.Runner = ExecRunner{}
	}
	if i.LookPath == nil {
		i.LookPath = exec.LookPath
	}
	if i.Environ == nil {
		i.Environ = os.Environ
	}
	if i.Logger == nil {
		i.Logger = slog.New(slog.DiscardHandler)
	}
	if i.BackoffMin == 0 {
		i.BackoffMin = 2 * time.Second
	}
	if i.BackoffMax == 0 {
		i.BackoffMax = 30 * time.Second
	}
	if i.Budget == 0 {
		i.Budget = DownloadBudget
	}
}

// Install finds or installs the runtime and the adapter of vendor. The
// returned error is an *Error.
func (i *Installer) Install(ctx context.Context, vendor sandcrewv1.AgentVendor) (Result, error) {
	i.defaults()
	spec, ok := i.Pins.Vendor(vendor)
	if !ok {
		return Result{}, installFailed("no pinned agent for %s", vendor)
	}
	if !spec.Supports(i.Target) {
		return Result{}, installFailed("%s has no pinned agent for %s", vendor, i.Target)
	}
	runtimePins := i.Pins.Runtimes[spec.Runtime]
	archive, ok := runtimePins.Targets[i.Target]
	if !ok {
		return Result{}, installFailed("runtime %s has no archive for %s", spec.Runtime, i.Target)
	}
	ctx, cancel := context.WithTimeout(ctx, i.Budget)
	defer cancel()

	var result Result
	runtimeRoot, preinstalled := i.findRuntime(ctx, runtimePins, archive)
	if preinstalled {
		result.RuntimePreinstalled = true
		i.Logger.Info("using preinstalled runtime", "runtime", spec.Runtime, "version", runtimePins.Version)
	} else {
		var err error
		if runtimeRoot, err = i.installRuntime(ctx, spec.Runtime, runtimePins, archive); err != nil {
			return Result{}, err
		}
	}
	result.RuntimePath = filepath.Join(runtimeRoot, filepath.FromSlash(archive.Executable))

	if preinstalled {
		globalModules := filepath.Join(runtimeRoot, filepath.FromSlash(archive.GlobalModules))
		pkgDir := filepath.Join(globalModules, filepath.FromSlash(spec.Adapter.Name))
		// The adapter depends on the agent through a version range, so a
		// global installation can resolve another agent version than the
		// embedded lockfile. Both must match the pins; hello reports them.
		adapterVersion := packageVersion(pkgDir)
		agentVersion := resolvedVersion(pkgDir, globalModules, spec.Agent.Name)
		if adapterVersion == spec.Adapter.Version && agentVersion == spec.Agent.Version {
			result.AdapterPreinstalled = true
			result.AdapterEntry = filepath.Join(pkgDir, filepath.FromSlash(spec.Adapter.Entry))
			i.Logger.Info("using preinstalled adapter", "adapter", spec.Adapter.Name, "version", spec.Adapter.Version, "agent", spec.Agent.Name, "agent_version", agentVersion)
			return result, nil
		}
		if adapterVersion != "" {
			i.Logger.Info("preinstalled adapter does not match the pins", "adapter_version", adapterVersion, "agent_version", agentVersion,
				"pinned_adapter_version", spec.Adapter.Version, "pinned_agent_version", spec.Agent.Version)
		}
	}
	pkgRoot, err := i.installAdapter(ctx, spec, result.RuntimePath, filepath.Join(runtimeRoot, filepath.FromSlash(archive.NpmCli)))
	if err != nil {
		return Result{}, err
	}
	result.AdapterEntry = filepath.Join(pkgRoot, "node_modules", filepath.FromSlash(spec.Adapter.Name), filepath.FromSlash(spec.Adapter.Entry))
	return result, nil
}

// resolvedVersion returns the version of the package name that Node.js
// resolves from the package in pkgDir inside the global node_modules
// directory globalModules: a copy nested in pkgDir/node_modules first, then
// the hoisted copy in globalModules. It returns "" when neither exists.
func resolvedVersion(pkgDir, globalModules, name string) string {
	for _, dir := range []string{
		filepath.Join(pkgDir, "node_modules", filepath.FromSlash(name)),
		filepath.Join(globalModules, filepath.FromSlash(name)),
	} {
		if _, err := os.Stat(filepath.Join(dir, "package.json")); err == nil {
			return packageVersion(dir)
		}
	}
	return ""
}

// findRuntime returns the root of a preinstalled runtime whose reported
// version equals the pin and that has npm at the archive layout.
func (i *Installer) findRuntime(ctx context.Context, runtimePins pins.Runtime, archive pins.RuntimeArchive) (string, bool) {
	name := strings.TrimSuffix(filepath.Base(filepath.FromSlash(archive.Executable)), ".exe")
	path, err := i.LookPath(name)
	if err != nil {
		return "", false
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	versionCtx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	out, err := i.Runner.Run(versionCtx, "", i.Environ(), path, "--version")
	if err != nil || strings.TrimSpace(string(out)) != runtimePins.ReportedVersion {
		i.Logger.Info("preinstalled runtime does not match the pin", "found", strings.TrimSpace(string(out)), "pinned", runtimePins.ReportedVersion)
		return "", false
	}
	root := path
	for range strings.Split(archive.Executable, "/") {
		root = filepath.Dir(root)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(archive.NpmCli))); err != nil {
		i.Logger.Info("preinstalled runtime has no npm at the pinned layout")
		return "", false
	}
	return root, true
}

func (i *Installer) installRuntime(ctx context.Context, name pins.RuntimeName, runtimePins pins.Runtime, archive pins.RuntimeArchive) (string, error) {
	parent := filepath.Join(i.AgentsDir, string(name))
	final := filepath.Join(parent, runtimePins.Version)
	if marker(final) == archive.SHA256 {
		return filepath.Join(final, filepath.FromSlash(archive.Root)), nil
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", installFailed("create %s: %w", parent, err)
	}
	download := filepath.Join(parent, ".download-"+randomSuffix())
	defer os.Remove(download)
	if err := i.download(ctx, archive.URL, archive.SHA256, download); err != nil {
		return "", err
	}
	staging := filepath.Join(parent, ".staging-"+randomSuffix())
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return "", installFailed("create %s: %w", staging, err)
	}
	defer fsutil.RemoveAll(staging)
	if err := extract(ctx, archive.Format, download, staging); err != nil {
		return "", installFailed("extract %s: %w", archive.URL, err)
	}
	if err := writeMarker(staging, archive.SHA256); err != nil {
		return "", installFailed("mark %s: %w", final, err)
	}
	if err := fsutil.RemoveAll(final); err != nil {
		return "", installFailed("remove stale %s: %w", final, err)
	}
	if err := os.Rename(staging, final); err != nil {
		return "", installFailed("install %s: %w", final, err)
	}
	i.Logger.Info("installed runtime", "runtime", name, "version", runtimePins.Version)
	return filepath.Join(final, filepath.FromSlash(archive.Root)), nil
}

// download fetches url into path with Attempts attempts and verifies the
// SHA-256 of the complete body. A digest mismatch is not retried.
func (i *Installer) download(ctx context.Context, url, sha256Hex, path string) error {
	delays := backoff.Backoff{Min: i.BackoffMin, Max: i.BackoffMax}
	var lastErr error
	for attempt := 1; attempt <= Attempts; attempt++ {
		if attempt > 1 {
			if err := backoff.Sleep(ctx, delays.Next()); err != nil {
				break
			}
		}
		digest, err := i.fetch(ctx, url, path)
		if err == nil {
			if digest != sha256Hex {
				return checksumMismatch("%s has SHA-256 %s, want %s", url, digest, sha256Hex)
			}
			return nil
		}
		lastErr = err
		i.Logger.Warn("download failed", "url", url, "attempt", attempt, "error", err)
		if ctx.Err() != nil {
			break
		}
	}
	if lastErr == nil {
		lastErr = ctx.Err()
	}
	return installFailed("download %s: %w", url, lastErr)
}

func (i *Installer) fetch(ctx context.Context, url, path string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := i.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP status %d", resp.StatusCode)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, hash), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (i *Installer) installAdapter(ctx context.Context, spec pins.Vendor, nodePath, npmCli string) (string, error) {
	packageJSON, packageLock, err := pins.NpmFiles(spec.PackageDir)
	if err != nil {
		return "", installFailed("read the embedded lockfile: %w", err)
	}
	lockSum := sha256.Sum256(packageLock)
	lockDigest := hex.EncodeToString(lockSum[:])
	final := filepath.Join(i.AgentsDir, spec.PackageDir, spec.Adapter.Version)
	if marker(final) == lockDigest {
		return final, nil
	}
	if err := fsutil.RemoveAll(final); err != nil {
		return "", installFailed("remove stale %s: %w", final, err)
	}
	if err := os.MkdirAll(final, 0o700); err != nil {
		return "", installFailed("create %s: %w", final, err)
	}
	if err := os.WriteFile(filepath.Join(final, "package.json"), packageJSON, 0o600); err != nil {
		return "", installFailed("write package.json: %w", err)
	}
	if err := os.WriteFile(filepath.Join(final, "package-lock.json"), packageLock, 0o600); err != nil {
		return "", installFailed("write package-lock.json: %w", err)
	}
	env := append(i.Environ(),
		"PATH="+filepath.Dir(nodePath)+string(os.PathListSeparator)+os.Getenv("PATH"),
		"npm_config_cache="+filepath.Join(i.AgentsDir, "npm-cache"),
		"npm_config_update_notifier=false",
		"npm_config_fund=false",
		"npm_config_audit=false",
	)
	delays := backoff.Backoff{Min: i.BackoffMin, Max: i.BackoffMax}
	var lastErr error
	for attempt := 1; attempt <= Attempts; attempt++ {
		if attempt > 1 {
			if err := backoff.Sleep(ctx, delays.Next()); err != nil {
				break
			}
		}
		out, err := i.Runner.Run(ctx, final, env, nodePath, npmCli, "ci", "--ignore-scripts", "--no-audit", "--no-fund", "--loglevel=error")
		if err == nil {
			if err := writeMarker(final, lockDigest); err != nil {
				return "", installFailed("mark %s: %w", final, err)
			}
			i.Logger.Info("installed adapter", "adapter", spec.Adapter.Name, "version", spec.Adapter.Version)
			return final, nil
		}
		// npm reports an integrity mismatch of a package as EINTEGRITY.
		if strings.Contains(string(out), "EINTEGRITY") {
			return "", checksumMismatch("npm ci of %s %s: a package does not match its lockfile integrity", spec.Adapter.Name, spec.Adapter.Version)
		}
		lastErr = fmt.Errorf("%w: %s", err, lastLine(out))
		i.Logger.Warn("npm ci failed", "adapter", spec.Adapter.Name, "attempt", attempt, "error", lastErr)
		if ctx.Err() != nil {
			break
		}
	}
	if lastErr == nil {
		lastErr = ctx.Err()
	}
	return "", installFailed("npm ci of %s %s: %w", spec.Adapter.Name, spec.Adapter.Version, lastErr)
}

func extract(ctx context.Context, format pins.ArchiveFormat, archivePath, dir string) error {
	switch format {
	case pins.ArchiveTarGz:
		f, err := os.Open(archivePath)
		if err != nil {
			return err
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gz.Close()
		return fsutil.ReadTarContext(ctx, gz, dir)
	case pins.ArchiveZip:
		return extractZip(ctx, archivePath, dir)
	default:
		return fmt.Errorf("unsupported archive format %q", format)
	}
}

// extractZip extracts regular files and directories. Every name must stay
// inside dir.
func extractZip(ctx context.Context, archivePath, dir string) error {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer reader.Close()
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, file := range reader.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := filepath.FromSlash(file.Name)
		if !filepath.IsLocal(name) {
			return fmt.Errorf("zip entry %q leaves the target directory", file.Name)
		}
		mode := file.Mode()
		switch {
		case mode.IsDir():
			if err := root.MkdirAll(name, 0o700); err != nil {
				return err
			}
		case mode.IsRegular():
			if err := root.MkdirAll(filepath.Dir(name), 0o700); err != nil {
				return err
			}
			if err := extractZipFile(root, file, name, mode.Perm()|0o600); err != nil {
				return err
			}
		default:
			return fmt.Errorf("zip entry %q has unsupported type %s", file.Name, mode.Type())
		}
	}
	return nil
}

func extractZipFile(root *os.Root, file *zip.File, name string, perm fs.FileMode) error {
	src, err := file.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	_, err = io.Copy(dst, src)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	return err
}

func marker(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, markerName))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func writeMarker(dir, digest string) error {
	return os.WriteFile(filepath.Join(dir, markerName), []byte(digest+"\n"), 0o600)
}

func packageVersion(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return ""
	}
	var manifest struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(data, &manifest) != nil {
		return ""
	}
	return manifest.Version
}

func randomSuffix() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// lastLine returns the last nonempty line of npm output for the bridge log.
// npm output names packages and registry errors, not credentials.
func lastLine(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			if len(line) > 300 {
				line = line[:300]
			}
			return line
		}
	}
	return "no output"
}

// IsError reports whether err is an installation error and returns it.
func IsError(err error) (*Error, bool) {
	var installErr *Error
	ok := errors.As(err, &installErr)
	return installErr, ok
}
