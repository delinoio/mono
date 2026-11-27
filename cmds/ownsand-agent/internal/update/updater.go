// SPDX-License-Identifier: FSL-1.1-ALv2

package update

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/internal/semver"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

const maxManifestBytes = 1 << 20

// MaxArtifactBytes is the inclusive size limit for an update executable.
const MaxArtifactBytes = 512 << 20

const artifactRetryDelay = time.Second

// ErrInstalling rejects creates and snapshot transfers once the installation
// started.
var ErrInstalling = errors.New("update: an agent update is installing")

// ErrRunningRevoked rejects creates while the running version is revoked.
var ErrRunningRevoked = errors.New("update: the running agent version is revoked")

// ErrSuperseded rejects an update command that a newer update or withdrawal
// command overtook before it recorded its final state or queued its artifact.
var ErrSuperseded = errors.New("update: a newer update command superseded this one")

// ErrRemoving rejects new update commands during device removal.
var ErrRemoving = errors.New("update: device removal has started")

// ErrRemovalAborted reports that device removal interrupted an admitted update command.
var ErrRemovalAborted = errors.New("update: device removal interrupted the update")

// Installer replaces the agent executable.
type Installer interface {
	Install(artifactPath string) error
}

// ExecutableInstaller replaces the executable at Path.
type ExecutableInstaller struct {
	Path string

	// rename and goos are test seams. A nil rename uses os.Rename and an empty
	// goos uses runtime.GOOS. They let tests drive the Windows rename sequence
	// and its failures on any host. Production code leaves both unset.
	rename func(oldPath, newPath string) error
	goos   string
}

// Install copies the artifact next to the executable and renames it into
// place. On Windows the running executable is renamed away first, because
// Windows cannot replace a running executable. When the rename of the new
// executable onto Path then fails, Install renames the previous executable
// back to Path, so the next start still finds it.
func (i ExecutableInstaller) Install(artifactPath string) error {
	data, err := os.ReadFile(artifactPath)
	if err != nil {
		return err
	}
	rename := i.rename
	if rename == nil {
		rename = os.Rename
	}
	goos := i.goos
	if goos == "" {
		goos = runtime.GOOS
	}
	next := i.Path + ".new"
	if err := store.WriteFileAtomic(next, data, 0o755); err != nil {
		return err
	}
	if goos != "windows" {
		return rename(next, i.Path)
	}
	old := i.Path + ".old"
	os.Remove(old)
	if err := rename(i.Path, old); err != nil {
		return err
	}
	if err := rename(next, i.Path); err != nil {
		// Path is missing now and the previous bytes are at old. Put them back.
		if restoreErr := rename(old, i.Path); restoreErr != nil {
			slog.Error("agent update install failed and the previous executable was not restored",
				"path", i.Path, "previous_path", old, "install_error", err, "restore_error", restoreErr)
			return fmt.Errorf("update: install %q failed: %w; restoring the previous executable from %q failed, so %q is not restored: %v",
				i.Path, err, old, i.Path, restoreErr)
		}
		slog.Warn("agent update install failed and the previous executable was restored",
			"path", i.Path, "previous_path", old, "install_error", err)
		return err
	}
	return nil
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
	// Busy reports sandboxes in creating, running or paused state, and
	// sandboxes in deleting state whose VM stop is not confirmed. Snapshot
	// transfers are not part of it: they reserve the device with
	// Updater.BeginTransfer.
	Busy func() bool
	// Restart ends the agent process after an installation, so the service
	// manager starts the new executable. It runs under the updater lock and
	// must return promptly without calling the updater.
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
	removing       bool
	installDone    chan struct{} // Non-nil from installation admission until failure or restart.
	creating       int
	transfers      int
	wake           chan struct{}
	// issued is the last order handed out by NextOrder; applied is the order
	// of the newest update or withdrawal command that took effect. Commands
	// run in their own goroutines, so the order is what lets a late older
	// command, and the download that it started, lose against a newer one.
	issued  uint64
	applied uint64
}

// New removes abandoned downloads, confirms the recorded installation and
// restores a verified revocation that applies to the running version.
func New(opts Options) (*Updater, error) {
	if opts.HTTP == nil {
		opts.HTTP = http.DefaultClient
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	u := &Updater{opts: opts, state: ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_UP_TO_DATE, wake: make(chan struct{}, 1)}
	u.removeAbandonedArtifacts()
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
	if st != nil && revocationApplies(st.RunningRevocation, opts.Running) {
		u.revokedRunning = true
		u.state = ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_REVOKED_PENDING_IDLE
		u.target = st.RunningRevocation.TargetVersion
		opts.Logger.Info("restored agent update revocation", "running_version", opts.Running, "target_version", u.target)
	}
	return u, nil
}

func revocationApplies(record *store.RunningRevocation, running string) bool {
	if record == nil {
		return false
	}
	comparison, err := semver.CompareStrings(record.Version, running)
	return err == nil && comparison == 0
}

// These patterns match the file names from download and store.WriteFileAtomic,
// including names with newlines that older agents could download.
var (
	artifactName     = regexp.MustCompile(`(?s)^ownsand-agent-.+-[0-9]+$`)
	artifactTempName = regexp.MustCompile(`(?s)^\.ownsand-agent-.+-[0-9]+\.tmp-[0-9]+$`)
)

// The queue lives in memory, so a new process cannot install files left by
// an earlier one. Each state directory belongs to one running agent.
func (u *Updater) removeAbandonedArtifacts() {
	dir := u.opts.Store.Path(store.UpdateDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// On Windows, ReadDir also returns ErrNotExist for a regular file.
			if _, statErr := os.Lstat(dir); errors.Is(statErr, fs.ErrNotExist) {
				return
			}
		}
		u.opts.Logger.Warn("update directory listing failed", "error", err)
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || !(artifactName.MatchString(name) || artifactTempName.MatchString(name)) {
			continue
		}
		size := int64(-1)
		if info, err := entry.Info(); err == nil {
			size = info.Size()
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				u.opts.Logger.Warn("abandoned update artifact removal failed", "name", name, "error", err)
			}
			continue
		}
		u.opts.Logger.Info("abandoned update artifact removed", "name", name, "size_bytes", size)
	}
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
	if u.installDone != nil {
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

// BeginTransfer admits a snapshot export or import. Transfers and the
// installation use the same mutex. An admitted transfer blocks installation
// until its release function runs. Once an installation starts, BeginTransfer
// returns ErrInstalling before the caller does any transfer I/O.
func (u *Updater) BeginTransfer() (func(), error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.installDone != nil {
		return nil, ErrInstalling
	}
	u.transfers++
	var once sync.Once
	return func() {
		once.Do(func() {
			u.mu.Lock()
			u.transfers--
			u.mu.Unlock()
			u.poke()
		})
	}, nil
}

func (u *Updater) poke() {
	select {
	case u.wake <- struct{}{}:
	default:
	}
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

// FenceForRemoval closes update admission for the life of the process and
// drops the queued artifact. It returns the completion signal for an admitted
// installation, or nil if there is none.
func (u *Updater) FenceForRemoval() <-chan struct{} {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.removing = true
	u.clearPendingLocked()
	return u.installDone
}

// commandInvalidationLocked checks an already admitted update command.
func (u *Updater) commandInvalidationLocked(order uint64) error {
	if u.removing {
		return ErrRemovalAborted
	}
	if u.applied != order {
		return ErrSuperseded
	}
	return nil
}

// setStateFor updates the state only while the command remains current.
func (u *Updater) setStateFor(order uint64, s ownsandv1.AgentUpdateState, target string) error {
	u.mu.Lock()
	if err := u.commandInvalidationLocked(order); err != nil {
		u.mu.Unlock()
		return err
	}
	// Only a verified manifest can replace the recovery target or lift the gate.
	if u.revokedRunning {
		u.mu.Unlock()
		return nil
	}
	u.state, u.target = s, target
	u.mu.Unlock()
	u.opts.Logger.Info("agent update state", "update_state", s.String(), "target_version", target)
	return nil
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

// recordRunningRevocation saves the verified decision under the command
// lock before applying it and fetching the artifact.
func (u *Updater) recordRunningRevocation(order uint64, revoked bool, target string) error {
	u.mu.Lock()
	if err := u.commandInvalidationLocked(order); err != nil {
		u.mu.Unlock()
		return err
	}
	state := ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_DOWNLOADING
	if revoked {
		state = ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_REVOKED_PENDING_IDLE
	}
	if revoked || u.revokedRunning {
		if _, err := u.opts.Store.UpdateAgent(func(a *store.AgentState) error {
			if revoked {
				a.RunningRevocation = &store.RunningRevocation{Version: u.opts.Running, TargetVersion: target}
			} else if revocationApplies(a.RunningRevocation, u.opts.Running) {
				a.RunningRevocation = nil
			}
			return nil
		}); err != nil {
			// A verified revocation must close the gate even if it cannot
			// survive a restart. Keep any previous recovery target on failure.
			if revoked {
				u.keepRunningRevocationLocked(target)
			}
			u.mu.Unlock()
			return fmt.Errorf("update: save running-version revocation: %w", err)
		}
	}
	u.revokedRunning = revoked
	u.state, u.target = state, target
	u.mu.Unlock()
	u.opts.Logger.Info("agent update state", "update_state", state.String(), "target_version", target)
	return nil
}

// keepRunningRevocationLocked closes the create gate without changing a
// previously recorded recovery target. The caller holds u.mu.
func (u *Updater) keepRunningRevocationLocked(target string) {
	if !u.revokedRunning {
		u.revokedRunning = true
		u.state, u.target = ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_REVOKED_PENDING_IDLE, target
	}
}

type downloadStatusError int

func (e downloadStatusError) Error() string {
	return fmt.Sprintf("update: download returned HTTP %d", e)
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
		return nil, downloadStatusError(resp.StatusCode)
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

func transientArtifactError(err error) bool {
	var status downloadStatusError
	if errors.As(err, &status) {
		switch int(status) {
		case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError,
			http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		default:
			return false
		}
	}
	var networkError net.Error
	return (errors.As(err, &networkError) && networkError.Timeout()) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) || transientSocketError(err)
}

func (u *Updater) fetchArtifact(ctx context.Context, url, target string, order uint64) ([]byte, error) {
	data, err := u.fetch(ctx, url, MaxArtifactBytes)
	if err == nil || !transientArtifactError(err) {
		return data, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	u.opts.Logger.Info("agent update artifact download will retry", "target_version", target, "delay", artifactRetryDelay)
	timer := time.NewTimer(artifactRetryDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
	}

	u.mu.Lock()
	invalidated := u.commandInvalidationLocked(order)
	u.mu.Unlock()
	if invalidated != nil {
		return nil, invalidated
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return u.fetch(ctx, url, MaxArtifactBytes)
}

// Handle runs an update command up to the verified download. The
// installation waits until the device is idle; Run performs it.
func (u *Updater) Handle(ctx context.Context, cmd *ownsandv1.UpdateAgentCommand) error {
	return u.HandleOrdered(ctx, cmd, u.NextOrder())
}

// HandleOrdered is Handle for a command with an order from NextOrder.
func (u *Updater) HandleOrdered(ctx context.Context, cmd *ownsandv1.UpdateAgentCommand, order uint64) error {
	target := cmd.GetTargetVersion()
	u.mu.Lock()
	if u.removing {
		u.mu.Unlock()
		return ErrRemoving
	}
	if u.installDone != nil {
		u.mu.Unlock()
		return ErrInstalling
	}
	if order < u.applied {
		u.mu.Unlock()
		u.opts.Logger.Info("agent update command superseded", "target_version", target, "order", order)
		return ErrSuperseded
	}
	comparison, comparisonErr := semver.CompareStrings(target, u.opts.Running)
	// A pin to the running version acts as a withdrawal. Keep signed
	// revocation recovery even if the control plane has not learned of it.
	if u.revokedRunning && comparisonErr == nil && comparison == 0 {
		u.mu.Unlock()
		return ErrRunningRevoked
	}
	// An admitted command supersedes the queued artifact. A failed or
	// same-version target must not install the stale artifact.
	u.applied = order
	u.clearPendingLocked()
	u.mu.Unlock()
	return u.prepareAdmittedUpdate(ctx, cmd, order, comparison, comparisonErr)
}

func (u *Updater) prepareAdmittedUpdate(ctx context.Context, cmd *ownsandv1.UpdateAgentCommand, order uint64, comparison int, comparisonErr error) error {
	target := cmd.GetTargetVersion()
	// An invalid target still clears the queued artifact if its command
	// passed the order check.
	if _, err := semver.Parse(target); err != nil {
		if invalidated := u.setStateFor(order, ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_FAILED, target); invalidated != nil {
			return invalidated
		}
		return err
	}
	if comparisonErr == nil && comparison == 0 {
		return u.setStateFor(order, ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_UP_TO_DATE, "")
	}
	if u.opts.ManifestURL == "" {
		if err := u.setStateFor(order, ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_FAILED, target); err != nil {
			return err
		}
		return errors.New("update: no release manifest URL is configured")
	}
	if err := u.setStateFor(order, ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_DOWNLOADING, target); err != nil {
		return err
	}
	data, err := u.download(ctx, target, order)
	if err == nil {
		err = u.queueVerified(ctx, order, target, data)
	}
	if err != nil {
		if invalidated := u.setStateFor(order, ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_FAILED, target); invalidated != nil {
			return invalidated
		}
		u.opts.Logger.Error("agent update preparation failed", "target_version", target, "error", err)
		return err
	}
	// The control plane decides lower targets: a pinned version or a
	// revocation recovery. The agent cannot see the organization policy,
	// so it installs the verified target that the control plane sent.
	if comparisonErr == nil && comparison < 0 {
		u.opts.Logger.Info("agent update installs a lower version", "target_version", target, "revocation_recovery", cmd.GetRevocationRecovery())
	}
	return nil
}

// queueVerified holds the updater lock through the disk write and queue
// publication. The last context check decides whether the update can pass
// from the channel command to the process-level installer.
func (u *Updater) queueVerified(ctx context.Context, order uint64, target string, data []byte) error {
	u.mu.Lock()
	if err := u.commandInvalidationLocked(order); err != nil {
		u.mu.Unlock()
		u.opts.Logger.Info("agent update download discarded", "target_version", target, "order", order)
		return err
	}
	if err := ctx.Err(); err != nil {
		u.mu.Unlock()
		return err
	}
	// Each command has a unique artifact path.
	path := u.opts.Store.Path(store.UpdateDir, fmt.Sprintf("ownsand-agent-%s-%d", target, order))
	if err := store.WriteFileAtomic(path, data, 0o700); err != nil {
		u.mu.Unlock()
		return err
	}
	if err := ctx.Err(); err != nil {
		u.mu.Unlock()
		if cleanupErr := os.Remove(path); cleanupErr != nil && !errors.Is(cleanupErr, fs.ErrNotExist) {
			u.opts.Logger.Warn("canceled update artifact removal failed", "target_version", target, "order", order, "error", cleanupErr)
		}
		return err
	}
	u.artifact = path
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
	if u.removing {
		u.mu.Unlock()
		return ErrRemoving
	}
	if u.installDone != nil {
		// Capture the target while holding u.mu.
		target := u.target
		u.mu.Unlock()
		u.opts.Logger.Warn("agent update withdrawal too late", "reason", "installing", "target_version", target)
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

type manifestRejectReason string

const (
	manifestOlderSequence      manifestRejectReason = "older_sequence"
	manifestConflictingContent manifestRejectReason = "conflicting_content"
	manifestMissingSequence    manifestRejectReason = "missing_sequence"
)

// A callback error skips the atomic write in Store.UpdateAgent.
var errManifestAlreadyAccepted = errors.New("update: release manifest checkpoint already matches")

func (u *Updater) acceptManifest(raw []byte, m *Manifest, order uint64, target string) error {
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	var acceptedSequence uint64
	var reason manifestRejectReason
	var writeCheckpoint bool

	// Fence the write against device removal. Command order still governs
	// revocation and artifact use, but every verified download can advance
	// the checkpoint.
	u.mu.Lock()
	if u.removing {
		u.mu.Unlock()
		return ErrRemovalAborted
	}
	_, err := u.opts.Store.UpdateAgent(func(st *store.AgentState) error {
		accepted := st.ReleaseManifest
		if accepted != nil {
			acceptedSequence = accepted.Sequence
		}
		switch {
		case m.Sequence == 0:
			reason = manifestMissingSequence
			return ErrManifestNoSequence
		case m.Sequence < acceptedSequence:
			reason = manifestOlderSequence
			return ErrManifestRollback
		case m.Sequence == acceptedSequence:
			if digest == accepted.SHA256 {
				return errManifestAlreadyAccepted
			}
			reason = manifestConflictingContent
			return ErrManifestRollback
		default:
			st.ReleaseManifest = &store.ManifestCheckpoint{Sequence: m.Sequence, SHA256: digest}
			writeCheckpoint = true
			return nil
		}
	})
	// The store checked the sequence but could not save it. A current
	// command can still close the create gate for a verified revocation.
	if err != nil && writeCheckpoint && u.commandInvalidationLocked(order) == nil && m.Revoked(u.opts.Running) {
		u.keepRunningRevocationLocked(target)
	}
	u.mu.Unlock()

	switch {
	case errors.Is(err, errManifestAlreadyAccepted):
		return nil
	case reason != "":
		u.opts.Logger.Error("release manifest rejected", "manifest_sequence", m.Sequence,
			"accepted_sequence", acceptedSequence, "reason", reason)
		return err
	case err != nil:
		return fmt.Errorf("update: store release manifest checkpoint: %w", err)
	default:
		u.opts.Logger.Info("release manifest accepted", "manifest_sequence", m.Sequence)
		return nil
	}
}

func (u *Updater) download(ctx context.Context, target string, order uint64) ([]byte, error) {
	manifest, err := u.fetch(ctx, u.opts.ManifestURL, maxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("update: download manifest: %w", err)
	}
	sigText, err := u.fetch(ctx, u.opts.ManifestURL+".sig", 4096)
	if err != nil {
		return nil, fmt.Errorf("update: download manifest signature: %w", err)
	}
	sig, err := DecodeSignature(sigText)
	if err != nil {
		return nil, err
	}
	keys := append(append([]ed25519.PublicKey{}, BuiltinKeys...), u.opts.Keys...)
	m, err := Verify(manifest, sig, keys)
	if err != nil {
		return nil, err
	}
	if err := u.acceptManifest(manifest, m, order, target); err != nil {
		return nil, err
	}
	if err := u.recordRunningRevocation(order, m.Revoked(u.opts.Running), target); err != nil {
		return nil, err
	}
	if m.Revoked(target) {
		return nil, ErrRevoked
	}
	art, err := m.Artifact(target, u.opts.Platform)
	if err != nil {
		return nil, err
	}
	data, err := u.fetchArtifact(ctx, art.URL, target, order)
	if err != nil {
		return nil, fmt.Errorf("update: download artifact: %w", err)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != art.SHA256 {
		return nil, ErrHashMismatch
	}
	return data, nil
}

// TryInstall installs a verified update when the device is idle and no
// create or snapshot transfer runs. It reports whether it installed.
func (u *Updater) TryInstall() (bool, error) {
	u.mu.Lock()
	if u.removing || u.artifact == "" || u.installDone != nil || u.creating > 0 || u.transfers > 0 || (u.opts.Busy != nil && u.opts.Busy()) {
		u.mu.Unlock()
		return false, nil
	}
	u.installDone = make(chan struct{})
	u.state = ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_INSTALLING
	target, artifact := u.target, u.artifact
	u.artifact = "" // The admitted installation owns the file until it finishes.
	u.mu.Unlock()
	u.opts.Logger.Info("agent update installing", "target_version", target)
	if _, err := u.opts.Store.UpdateAgent(func(a *store.AgentState) error {
		a.PendingUpdate = &store.PendingUpdate{TargetVersion: target, PreviousVersion: u.opts.Running}
		return nil
	}); err != nil {
		u.finishInstall(target, artifact, false)
		return false, err
	}
	if err := u.opts.Installer.Install(artifact); err != nil {
		u.opts.Store.UpdateAgent(func(a *store.AgentState) error { a.PendingUpdate = nil; return nil })
		u.finishInstall(target, artifact, false)
		return false, fmt.Errorf("update: install: %w", err)
	}
	u.finishInstall(target, artifact, true)
	return true, nil
}

// finishInstall signals completion after the installer, its state writes and
// its artifact cleanup have finished.
func (u *Updater) finishInstall(target, artifact string, installed bool) {
	os.Remove(filepath.Clean(artifact))
	u.mu.Lock()
	defer u.mu.Unlock()
	defer close(u.installDone)
	if installed {
		if u.removing {
			u.opts.Logger.Info("agent update installed during device removal", "target_version", target)
		} else {
			u.opts.Logger.Info("agent update installed; restarting", "target_version", target)
			if u.opts.Restart != nil {
				u.opts.Restart()
			}
		}
		return
	}
	u.installDone = nil
	// revokedRunning is deliberately kept: it describes the running version
	// from a verified manifest, not the failed install attempt, so creates stay
	// blocked until a later install replaces the revoked version. The reported
	// state follows it: a device that runs a revoked version stays
	// REVOKED_PENDING_IDLE after the failure so the control plane keeps
	// placing no sandboxes on it, as for a failed download.
	if u.revokedRunning {
		u.state = ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_REVOKED_PENDING_IDLE
	} else {
		u.state = ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_FAILED
	}
	u.target = target
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
