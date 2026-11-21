// SPDX-License-Identifier: Apache-2.0

package update

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/internal/semver"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// maxDownload bounds the manifest and artifact downloads.
const (
	maxManifestBytes = 1 << 20
	maxArtifactBytes = 512 << 20
)

// ErrInstalling rejects creates once the installation started.
var ErrInstalling = errors.New("update: an agent update is installing")

// ErrRunningRevoked rejects creates while the running version is revoked.
var ErrRunningRevoked = errors.New("update: the running agent version is revoked")

// ErrSuperseded rejects an update command that a newer update or withdrawal
// command overtook before it published its artifact.
var ErrSuperseded = errors.New("update: a newer update command superseded this one")

// Installer replaces the agent executable.
type Installer interface {
	Install(artifactPath string) error
}

// ExecutableInstaller replaces the executable at Path.
type ExecutableInstaller struct {
	Path string
}

// Install copies the artifact next to the executable and renames it into
// place. On Windows the running executable is renamed away first, because
// Windows cannot replace a running executable.
func (i ExecutableInstaller) Install(artifactPath string) error {
	data, err := os.ReadFile(artifactPath)
	if err != nil {
		return err
	}
	next := i.Path + ".new"
	if err := store.WriteFileAtomic(next, data, 0o755); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		old := i.Path + ".old"
		os.Remove(old)
		if err := os.Rename(i.Path, old); err != nil {
			return err
		}
	}
	return os.Rename(next, i.Path)
}

// Options configure an Updater.
type Options struct {
	Store       *store.Store
	HTTP        *http.Client
	ManifestURL string
	Keys        []ed25519.PublicKey
	Platform    string
	Running     string
	Installer   Installer
	// Busy reports sandboxes in creating, running or paused state or a
	// running snapshot transfer.
	Busy func() bool
	// Restart ends the agent process after an installation, so the service
	// manager starts the new executable.
	Restart func()
	Logger  *slog.Logger
}

// Updater runs the update flow. It is safe for concurrent use.
type Updater struct {
	opts Options

	mu             sync.Mutex
	state          ownsandv1.AgentUpdateState
	target         string
	artifact       string
	revokedRunning bool
	installing     bool
	creating       int
	wake           chan struct{}
	// issued is the last order handed out by NextOrder; applied is the order
	// of the newest update or withdrawal command that took effect. Commands
	// run in their own goroutines, so the order is what lets a late older
	// command, and the download that it started, lose against a newer one.
	issued  uint64
	applied uint64
}

// New returns an updater. It confirms an installation of the previous
// process: the running version must equal the recorded target.
func New(opts Options) (*Updater, error) {
	if opts.HTTP == nil {
		opts.HTTP = http.DefaultClient
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	u := &Updater{opts: opts, state: ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_UP_TO_DATE, wake: make(chan struct{}, 1)}
	st, err := opts.Store.LoadAgent()
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if st != nil && st.PendingUpdate != nil {
		pending := *st.PendingUpdate
		if c, err := semver.CompareStrings(pending.TargetVersion, opts.Running); err == nil && c == 0 {
			opts.Logger.Info("agent update installed", "version", opts.Running, "previous_version", pending.PreviousVersion)
		} else {
			u.state = ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_FAILED
			u.target = pending.TargetVersion
			opts.Logger.Error("agent update did not take effect", "target_version", pending.TargetVersion, "running_version", opts.Running)
		}
		if _, err := opts.Store.UpdateAgent(func(a *store.AgentState) error { a.PendingUpdate = nil; return nil }); err != nil {
			return nil, err
		}
	}
	return u, nil
}

// State returns the update state and the target version for heartbeats.
func (u *Updater) State() (ownsandv1.AgentUpdateState, string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.state, u.target
}

// BeginCreate admits a create command. Creates and the installation are
// mutually exclusive.
func (u *Updater) BeginCreate() (func(), error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.installing {
		return nil, ErrInstalling
	}
	if u.revokedRunning {
		return nil, ErrRunningRevoked
	}
	u.creating++
	return func() {
		u.mu.Lock()
		u.creating--
		u.mu.Unlock()
		u.poke()
	}, nil
}

func (u *Updater) poke() {
	select {
	case u.wake <- struct{}{}:
	default:
	}
}

func (u *Updater) setState(s ownsandv1.AgentUpdateState, target string) {
	u.mu.Lock()
	u.state, u.target = s, target
	u.mu.Unlock()
	u.opts.Logger.Info("agent update state", "update_state", s.String(), "target_version", target)
}

// NextOrder returns the order of the next update or withdrawal command. The
// agent takes it synchronously when it receives the command, before the
// command runs in its own goroutine, so the order is the receive order.
func (u *Updater) NextOrder() uint64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.issued++
	return u.issued
}

// setStateFor sets the state on behalf of the command with the given order
// and reports whether it did. A command that a newer one superseded must not
// overwrite the state of the newer command.
func (u *Updater) setStateFor(order uint64, s ownsandv1.AgentUpdateState, target string) bool {
	u.mu.Lock()
	if u.applied != order {
		u.mu.Unlock()
		return false
	}
	u.state, u.target = s, target
	u.mu.Unlock()
	u.opts.Logger.Info("agent update state", "update_state", s.String(), "target_version", target)
	return true
}

// clearPendingLocked drops the queued artifact and removes its downloaded
// file. The caller holds u.mu. revokedRunning is kept: it describes the
// running version, not the queued artifact.
func (u *Updater) clearPendingLocked() {
	if u.artifact == "" {
		return
	}
	os.Remove(filepath.Clean(u.artifact))
	u.artifact = ""
}

func (u *Updater) fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := u.opts.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("update: download returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("update: download is larger than its limit")
	}
	return data, nil
}

// Handle runs an update command up to the verified download. The
// installation waits until the device is idle; Run performs it.
func (u *Updater) Handle(ctx context.Context, cmd *ownsandv1.UpdateAgentCommand) error {
	return u.HandleOrdered(ctx, cmd, u.NextOrder())
}

// HandleOrdered is Handle for a command with an order from NextOrder.
func (u *Updater) HandleOrdered(ctx context.Context, cmd *ownsandv1.UpdateAgentCommand, order uint64) error {
	target := cmd.GetTargetVersion()
	if _, err := semver.Parse(target); err != nil {
		u.setState(ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_FAILED, target)
		return err
	}
	u.mu.Lock()
	if u.installing {
		u.mu.Unlock()
		return ErrInstalling
	}
	if order < u.applied {
		u.mu.Unlock()
		u.opts.Logger.Info("agent update command superseded", "target_version", target, "order", order)
		return ErrSuperseded
	}
	// A new command supersedes the queued artifact, whatever its outcome:
	// a failed or same-version target must not install the stale artifact.
	u.applied = order
	u.clearPendingLocked()
	u.mu.Unlock()
	if c, _ := semver.CompareStrings(target, u.opts.Running); c == 0 {
		u.setStateFor(order, ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_UP_TO_DATE, "")
		return nil
	}
	if u.opts.ManifestURL == "" {
		u.setStateFor(order, ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_FAILED, target)
		return errors.New("update: no release manifest URL is configured")
	}
	if !u.setStateFor(order, ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_DOWNLOADING, target) {
		return ErrSuperseded
	}
	m, artifactPath, err := u.download(ctx, target, order)
	if err != nil {
		if u.setStateFor(order, ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_FAILED, target) {
			u.opts.Logger.Error("agent update verification failed", "target_version", target, "error", err)
			return err
		}
		return ErrSuperseded
	}
	// The control plane decides lower targets: a pinned version or a
	// revocation recovery. The agent cannot see the organization policy,
	// so it installs the verified target that the control plane sent.
	if c, _ := semver.CompareStrings(target, u.opts.Running); c < 0 {
		u.opts.Logger.Info("agent update installs a lower version", "target_version", target, "revocation_recovery", cmd.GetRevocationRecovery())
	}
	u.mu.Lock()
	if u.applied != order {
		// A newer update or a withdrawal took effect during the download:
		// drop the artifact instead of queueing it. The file name is unique
		// to this command, so this cannot remove the artifact of another one.
		u.mu.Unlock()
		os.Remove(filepath.Clean(artifactPath))
		u.opts.Logger.Info("agent update download discarded", "target_version", target, "order", order)
		return ErrSuperseded
	}
	u.revokedRunning = m.Revoked(u.opts.Running)
	u.artifact = artifactPath
	u.target = target
	if u.revokedRunning {
		u.state = ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_REVOKED_PENDING_IDLE
	} else {
		u.state = ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_PENDING_IDLE
	}
	state := u.state
	u.mu.Unlock()
	u.opts.Logger.Info("agent update verified", "target_version", target, "update_state", state.String())
	u.poke()
	return nil
}

// Withdraw drops the queued update and invalidates a download that is still
// running, so the installer is not called for a target that the control
// plane withdrew. It runs under the updater lock, which TryInstall also
// holds when it starts the installation: a withdrawal before that point
// prevents the installation; after it, the withdrawal returns ErrInstalling
// and changes nothing, because an installation that started cannot be taken
// back. While the running version is revoked it also changes nothing and
// returns ErrRunningRevoked, because the revocation recovery must proceed.
func (u *Updater) Withdraw() error {
	return u.WithdrawOrdered(u.NextOrder())
}

// WithdrawOrdered is Withdraw for a command with an order from NextOrder.
func (u *Updater) WithdrawOrdered(order uint64) error {
	u.mu.Lock()
	if u.installing {
		u.mu.Unlock()
		u.opts.Logger.Warn("agent update withdrawal too late", "reason", "installing", "target_version", u.target)
		return ErrInstalling
	}
	if u.revokedRunning {
		u.mu.Unlock()
		u.opts.Logger.Warn("agent update withdrawal ignored", "reason", "running_version_revoked", "running_version", u.opts.Running)
		return ErrRunningRevoked
	}
	if order < u.applied {
		// A newer update command already replaced the state that this
		// withdrawal refers to.
		u.mu.Unlock()
		u.opts.Logger.Info("agent update withdrawal superseded", "order", order)
		return nil
	}
	u.applied = order
	previous, target := u.state, u.target
	u.clearPendingLocked()
	u.state, u.target = ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_UP_TO_DATE, ""
	u.mu.Unlock()
	u.opts.Logger.Info("agent update withdrawn", "previous_update_state", previous.String(), "withdrawn_target_version", target)
	return nil
}

func (u *Updater) download(ctx context.Context, target string, order uint64) (*Manifest, string, error) {
	manifest, err := u.fetch(ctx, u.opts.ManifestURL, maxManifestBytes)
	if err != nil {
		return nil, "", fmt.Errorf("update: download manifest: %w", err)
	}
	sigText, err := u.fetch(ctx, u.opts.ManifestURL+".sig", 4096)
	if err != nil {
		return nil, "", fmt.Errorf("update: download manifest signature: %w", err)
	}
	sig, err := DecodeSignature(sigText)
	if err != nil {
		return nil, "", err
	}
	keys := append(append([]ed25519.PublicKey{}, BuiltinKeys...), u.opts.Keys...)
	m, err := Verify(manifest, sig, keys)
	if err != nil {
		return nil, "", err
	}
	if m.Revoked(target) {
		return nil, "", ErrRevoked
	}
	art, err := m.Artifact(target, u.opts.Platform)
	if err != nil {
		return nil, "", err
	}
	data, err := u.fetch(ctx, art.URL, maxArtifactBytes)
	if err != nil {
		return nil, "", fmt.Errorf("update: download artifact: %w", err)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != art.SHA256 {
		return nil, "", ErrHashMismatch
	}
	// The order makes the file name unique to the command, so a superseded
	// download never removes or replaces the artifact of a newer command.
	path := u.opts.Store.Path(store.UpdateDir, fmt.Sprintf("ownsand-agent-%s-%d", target, order))
	if err := store.WriteFileAtomic(path, data, 0o700); err != nil {
		return nil, "", err
	}
	return m, path, nil
}

// TryInstall installs a verified update when the device is idle and no
// create runs. It reports whether it installed.
func (u *Updater) TryInstall() (bool, error) {
	u.mu.Lock()
	if u.artifact == "" || u.installing || u.creating > 0 || (u.opts.Busy != nil && u.opts.Busy()) {
		u.mu.Unlock()
		return false, nil
	}
	u.installing = true
	u.state = ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_INSTALLING
	target, artifact := u.target, u.artifact
	u.mu.Unlock()
	u.opts.Logger.Info("agent update installing", "target_version", target)
	if _, err := u.opts.Store.UpdateAgent(func(a *store.AgentState) error {
		a.PendingUpdate = &store.PendingUpdate{TargetVersion: target, PreviousVersion: u.opts.Running}
		return nil
	}); err != nil {
		u.fail(target)
		return false, err
	}
	if err := u.opts.Installer.Install(artifact); err != nil {
		u.opts.Store.UpdateAgent(func(a *store.AgentState) error { a.PendingUpdate = nil; return nil })
		u.fail(target)
		return false, fmt.Errorf("update: install: %w", err)
	}
	os.Remove(filepath.Clean(artifact))
	u.opts.Logger.Info("agent update installed; restarting", "target_version", target)
	if u.opts.Restart != nil {
		u.opts.Restart()
	}
	return true, nil
}

func (u *Updater) fail(target string) {
	u.mu.Lock()
	u.installing = false
	u.artifact = ""
	// revokedRunning is deliberately kept: it describes the running version
	// from a verified manifest, not the failed install attempt, so creates stay
	// blocked until a later install replaces the revoked version.
	u.state = ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_FAILED
	u.target = target
	u.mu.Unlock()
}

// Run tries the installation every 10 seconds and after each create.
func (u *Updater) Run(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-u.wake:
		}
		if _, err := u.TryInstall(); err != nil {
			u.opts.Logger.Error("agent update installation failed", "error", err)
		}
	}
}
