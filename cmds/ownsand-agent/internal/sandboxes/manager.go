// SPDX-License-Identifier: FSL-1.1-ALv2

// Package sandboxes keeps the sandbox and snapshot records of the device,
// runs the sandbox commands of the agent channel through a runtime,
// enforces the idle and maximum TTLs (also while the agent channel is
// down) and produces the state reports and sandbox updates with their
// revisions. The owning contracts are docs/cmds-ownsand-agent-foundation.md
// and docs/servers-ownsand-api-foundation.md (agent channel, TTL).
package sandboxes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/guestclient"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/sysinfo"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	"github.com/delinoio/mono/go/ownsand/validate"
	"github.com/delinoio/mono/go/uuidv7"
	"github.com/delinoio/mono/internal/fsutil"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Timing rules of the agent channel contract.
const (
	// IdleReportInterval: an idle deadline moved by activity is reported at
	// most once per minute for each sandbox.
	IdleReportInterval = time.Minute
	// ResendInterval: unacknowledged sandbox updates are sent again.
	ResendInterval = 10 * time.Second
	// MaxMacOSGuests is the Virtualization.framework limit.
	MaxMacOSGuests = 2
	// DefaultRunningAttempts is the number of commits that a create tries
	// to publish the RUNNING record after the VM started, before it stops
	// the VM again.
	DefaultRunningAttempts = 3
	// DefaultRunningBackoff is the wait after the first failed RUNNING
	// commit; the wait grows linearly with the attempt number.
	DefaultRunningBackoff = 200 * time.Millisecond
)

// Outbox sends sandbox updates on the current agent channel. It returns
// false when no channel is connected; the next state report covers the
// change.
type Outbox interface {
	SendSandboxUpdate(*ownsandv1.SandboxStateEntry) bool
}

// ExecTracker is the exec bookkeeping of the data path.
type ExecTracker interface {
	HasRunningExec(sandboxID string) bool
	SandboxEnded(sandboxID string, reason ownsandv1.SandboxEndReason)
}

// CreateGate admits creates; the update flow closes it while an update
// installs or while the running version is revoked.
type CreateGate interface {
	BeginCreate() (release func(), err error)
}

// CommandError is a command failure that becomes the ErrorDetail of a
// command result.
type CommandError struct {
	Code    ownsandv1.ErrorCode
	Message string
}

func (e *CommandError) Error() string { return e.Message }

// Detail returns the ErrorDetail of the command result.
func (e *CommandError) Detail() *ownsandv1.ErrorDetail {
	return &ownsandv1.ErrorDetail{Code: e.Code, Message: e.Message}
}

func cmdErr(code ownsandv1.ErrorCode, format string, args ...any) *CommandError {
	return &CommandError{Code: code, Message: fmt.Sprintf(format, args...)}
}

type sandbox struct {
	rec Record
	// cmdMu serializes the commands of the sandbox.
	cmdMu        sync.Mutex
	vm           vmruntime.VM
	guest        *guestclient.Client
	createCancel context.CancelFunc
	// ending is true while the agent itself stops the VM.
	ending bool
	// stopped is true once an end attempt stopped the VM and told the exec
	// tracker, so a retry after a later failure does not repeat them.
	stopped        bool
	idleDirty      bool
	lastIdleReport time.Time
}

// Options configure a Manager.
type Options struct {
	Store   *store.Store
	Runtime vmruntime.Runtime
	Logger  *slog.Logger
	Now     func() time.Time
	// FreeDisk returns the free bytes of the state directory; nil uses
	// sysinfo.
	FreeDisk func() (int64, error)
}

// Manager owns the sandboxes and snapshots of the device. It is safe for
// concurrent use.
type Manager struct {
	st       *store.Store
	rt       vmruntime.Runtime
	logger   *slog.Logger
	now      func() time.Time
	freeDisk func() (int64, error)

	mu        sync.Mutex
	epoch     string
	sandboxes map[string]*sandbox
	snapshots map[string]*snapshot
	// pending holds the snapshots with a received create command that has
	// not ended, by ID: the create waits for its sandbox or captures. They
	// are not necessarily in snapshots, so they are not usable and not
	// reported. See SnapshotCreateClaim.
	pending map[string]*pendingSnapshot
	// importLocks serializes the imports of one snapshot ID; see
	// LockSnapshotImport.
	importLocks map[string]*importLock
	// removing is the device-removal fence: RemoveAll sets it before it
	// waits for the running snapshot imports and wipes the local data, and
	// nothing clears it, because the removal notice is final (a failed
	// removal retries in the same process or after a restart that sets the
	// fence again). While it is set, BeginSnapshotImport and
	// AddImportedSnapshot refuse, so that no import creates snapshot
	// metadata or directories after the wipe. imports counts the imports
	// that passed BeginSnapshotImport and did not end; importsDrained is
	// closed (and cleared) when the count reaches zero while a RemoveAll
	// waits for it.
	removing       bool
	imports        int
	importsDrained chan struct{}
	limits         *ownsandv1.DeviceLimits
	reports        map[uint64]map[string]uint64
	outbox         Outbox
	execs          ExecTracker
	gate           CreateGate
	closed         bool
	// cleanupPending holds the IDs of the sandboxes whose create failed in
	// the runtime. The runtime reports a failed create as clean, but a
	// runtime whose stop failed keeps the VM handle and files for a later
	// stop (see the runtime Create contract); Tick calls the idempotent
	// runtime Delete for each ID until it succeeds. cleanupInFlight holds
	// the IDs whose Delete runs now. Both are in memory only: the next agent
	// start reconciles through the runtime.
	cleanupPending  map[string]struct{}
	cleanupInFlight map[string]struct{}
	// runningAttempts and runningBackoff bound the retries of the first
	// RUNNING commit of a create; see publishRunning. Tests override them.
	runningAttempts int
	runningBackoff  time.Duration
	// onRunningCommitFailed is a test hook that publishRunning calls after
	// a failed RUNNING commit, before it waits for the next attempt.
	onRunningCommitFailed func(attempt int, willRetry bool)
	// beforeIdleReports is a test hook that Tick calls after it collected the
	// pending idle reports and released mu, before it publishes them.
	beforeIdleReports func()
	// removalHook is a test hook that removeSnapshot calls under hookMu
	// before it deletes the stored copy; see SetSnapshotRemovalHookForTest.
	hookMu      sync.Mutex
	removalHook func(id string) error
	// bg tracks the background sandbox ends (TTL expiries and unexpected
	// VM exits). Add is called only under mu while closed is false, so
	// that Close can wait for them.
	bg sync.WaitGroup
}

// Close stops recording VM ends and starting TTL ends, and waits for the
// ends that already run, so that no record is written after Close
// returns. The agent calls it when it shuts down: its VMs end with the
// process, and the next start reconciles the records.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.bg.Wait()
}

// New loads the records, creates a state epoch when the state directory
// was lost or reset, and reconciles the records with the VMs that the
// runtime still runs.
func New(ctx context.Context, opts Options) (*Manager, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.FreeDisk == nil {
		dir := opts.Store.Dir()
		opts.FreeDisk = func() (int64, error) { return sysinfo.FreeDiskBytes(dir) }
	}
	m := &Manager{
		st: opts.Store, rt: opts.Runtime, logger: opts.Logger, now: opts.Now, freeDisk: opts.FreeDisk,
		runningAttempts: DefaultRunningAttempts, runningBackoff: DefaultRunningBackoff,
		sandboxes: map[string]*sandbox{}, snapshots: map[string]*snapshot{}, pending: map[string]*pendingSnapshot{}, cleanupPending: map[string]struct{}{}, cleanupInFlight: map[string]struct{}{}, importLocks: map[string]*importLock{}, reports: map[uint64]map[string]uint64{},
	}
	agentState, err := m.st.UpdateAgent(func(a *store.AgentState) error {
		if a.StateEpoch == "" {
			epoch, err := uuidv7.NewString()
			if err != nil {
				return err
			}
			a.StateEpoch = epoch
			a.RevisionCounter = 0
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("sandboxes: load agent state: %w", err)
	}
	m.epoch = agentState.StateEpoch
	if l := agentState.Limits; l != nil {
		m.limits = &ownsandv1.DeviceLimits{VcpuCount: l.VCPUCount, MemoryMib: l.MemoryMiB, DiskGib: l.DiskGiB, VcpuOvercommitPercent: l.VCPUOvercommitPercent}
	}
	if err := m.st.LoadSandboxes(func(data []byte) error {
		var rec Record
		if err := json.Unmarshal(data, &rec); err != nil {
			return err
		}
		m.sandboxes[rec.SandboxID] = &sandbox{rec: rec}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := m.loadSnapshots(); err != nil {
		return nil, err
	}
	if err := m.reconcile(ctx); err != nil {
		return nil, err
	}
	return m, nil
}

// SetOutbox sets the agent channel outbox.
func (m *Manager) SetOutbox(o Outbox) { m.mu.Lock(); m.outbox = o; m.mu.Unlock() }

// SetExecTracker sets the exec bookkeeping.
func (m *Manager) SetExecTracker(e ExecTracker) { m.mu.Lock(); m.execs = e; m.mu.Unlock() }

// SetCreateGate sets the gate of the update flow.
func (m *Manager) SetCreateGate(g CreateGate) { m.mu.Lock(); m.gate = g; m.mu.Unlock() }

// Epoch returns the state epoch.
func (m *Manager) Epoch() string { return m.epoch }

// reconcile handles records whose VM is gone after an agent restart: every
// VM is bound to the agent process, so a record in creating, running or
// paused state has lost its VM. A passed TTL wins over LOST_ON_DEVICE. VMs
// that survived (a failed binding) are stopped. The runtime also removes the
// files of every live record before the record ends, because a lost VM
// leaves its directory and disk on the device. When the removal fails, the
// record stays live and persisted, so the next start (or a TTL tick) tries
// again, and the terminal state is not reported before the files are gone.
func (m *Manager) reconcile(ctx context.Context) error {
	running, err := m.rt.Running(ctx)
	if err != nil {
		return fmt.Errorf("sandboxes: list runtime VMs: %w", err)
	}
	// cleaned holds the sandboxes whose runtime Delete already succeeded.
	cleaned := map[string]bool{}
	for _, id := range running {
		m.logger.Warn("stopping a VM that outlived its agent process", "sandbox_id", id)
		if err := m.rt.Delete(ctx, id); err != nil {
			m.logger.Error("stop surviving VM failed", "sandbox_id", id, "error", err)
			continue
		}
		cleaned[id] = true
	}
	now := m.now()
	for _, sb := range m.sandboxes {
		if !sb.rec.Live() {
			continue
		}
		id := sb.rec.SandboxID
		if !cleaned[id] {
			// Delete is idempotent: it removes the runtime files of a
			// sandbox whose VM is already gone.
			if err := m.rt.Delete(ctx, id); err != nil {
				m.logger.Error("remove runtime files of a lost sandbox failed; the record stays live for a retry", "sandbox_id", id, "error", err)
				continue
			}
			cleaned[id] = true
		}
		reason := sb.rec.endReasonAt(now)
		switch {
		case reason != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED:
		case sb.rec.State == ownsandv1.SandboxState_SANDBOX_STATE_DELETING:
			// A failed end attempt recorded its reason; the final state
			// follows from it, so a lost VM stays FAILED/LOST_ON_DEVICE.
			reason = sb.rec.EndReason
		default:
			reason = ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE
		}
		state := finalStateFor(reason)
		m.logger.Info("sandbox ended while the agent was stopped", "sandbox_id", id, "state", state.String(), "end_reason", reason.String())
		if err := m.commitLocked(sb, func(r *Record) {
			r.State = state
			r.EndReason = reason
		}); err != nil {
			return err
		}
	}
	return nil
}

// finalStateFor returns the terminal state that ends a sandbox with reason:
// FAILED for LOST_ON_DEVICE (the VM ended without a command) and DELETED for
// every other reason (a delete or a TTL end). The record in DELETING keeps
// its end reason across a failed end attempt, so an end that retries later,
// also after an agent restart, derives the same state without a new record
// field.
func finalStateFor(reason ownsandv1.SandboxEndReason) ownsandv1.SandboxState {
	if reason == ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE {
		return ownsandv1.SandboxState_SANDBOX_STATE_FAILED
	}
	return ownsandv1.SandboxState_SANDBOX_STATE_DELETED
}

// nextRevision persists and returns the next revision counter.
func (m *Manager) nextRevision() (uint64, error) {
	st, err := m.st.UpdateAgent(func(a *store.AgentState) error {
		a.RevisionCounter++
		return nil
	})
	if err != nil {
		return 0, err
	}
	return st.RevisionCounter, nil
}

// commitLocked applies change to the record with a new revision and saves
// it. The caller holds m.mu or has exclusive access.
func (m *Manager) commitLocked(sb *sandbox, change func(*Record)) error {
	rev, err := m.nextRevision()
	if err != nil {
		return fmt.Errorf("sandboxes: persist revision: %w", err)
	}
	change(&sb.rec)
	sb.rec.Revision = rev
	sb.rec.Acked = false
	if err := m.st.SaveSandbox(sb.rec.SandboxID, sb.rec); err != nil {
		return fmt.Errorf("sandboxes: save record: %w", err)
	}
	return nil
}

// errSandboxGone reports that updateIf skipped a stale sandbox: its guard
// rejected the sandbox, so nothing was committed or sent.
var errSandboxGone = errors.New("sandboxes: sandbox is gone or no longer live")

// update commits a change and sends the sandbox update.
func (m *Manager) update(sb *sandbox, change func(*Record)) (*ownsandv1.SandboxStateEntry, error) {
	return m.updateIf(sb, nil, change)
}

// updateIf is update with a guard that runs under m.mu before the commit. A
// nil guard always accepts. When the guard rejects, updateIf commits nothing
// and returns errSandboxGone. A caller that holds a *sandbox obtained under an
// earlier m.mu section uses the guard to verify that the sandbox is still
// the one in m.sandboxes, so that it cannot recreate a dropped record.
func (m *Manager) updateIf(sb *sandbox, guard func() bool, change func(*Record)) (*ownsandv1.SandboxStateEntry, error) {
	m.mu.Lock()
	if guard != nil && !guard() {
		m.mu.Unlock()
		return nil, errSandboxGone
	}
	before := sb.rec.State
	err := m.commitLocked(sb, change)
	entry := sb.rec.Entry(m.epoch)
	outbox := m.outbox
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if before != entry.GetState() {
		m.logger.Info("sandbox state changed", "sandbox_id", entry.GetSandboxId(), "state", entry.GetState().String(), "end_reason", entry.GetEndReason().String(), "revision", entry.GetRevision().GetCounter())
	}
	if outbox != nil {
		outbox.SendSandboxUpdate(entry)
	}
	return entry, nil
}

func (m *Manager) get(id string) (*sandbox, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sb, ok := m.sandboxes[id]
	return sb, ok
}

// State returns the state of a sandbox.
func (m *Manager) State(id string) (ownsandv1.SandboxState, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sb, ok := m.sandboxes[id]
	if !ok {
		return ownsandv1.SandboxState_SANDBOX_STATE_UNSPECIFIED, false
	}
	return sb.rec.State, true
}

// WithRunning calls fn while the sandbox is running and not ending, holding
// m.mu, and returns false without calling fn otherwise. Every end path sets
// sb.ending under m.mu before it notifies the exec tracker, so an exec that
// registers in fn is visible to that notification, and an exec that comes
// later sees the end and is refused. fn must not call the Manager. It may take
// the exec tracker's lock: Tick already calls the tracker under m.mu, and
// the tracker never calls the Manager while it holds that lock.
func (m *Manager) WithRunning(id string, fn func()) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	sb, ok := m.sandboxes[id]
	if !ok || sb.ending || sb.rec.State != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		return false
	}
	fn()
	return true
}

// Guest returns the guest helper client of a sandbox with a VM.
func (m *Manager) Guest(id string) (*guestclient.Client, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sb, ok := m.sandboxes[id]
	if !ok || sb.guest == nil {
		return nil, false
	}
	return sb.guest, true
}

// Touch records activity: the idle deadline of a running sandbox moves to
// now plus its idle TTL.
func (m *Manager) Touch(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sb, ok := m.sandboxes[id]
	if !ok || sb.rec.State != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		return
	}
	sb.rec.IdleDeadline = m.now().Add(sb.rec.IdleTTL)
	sb.idleDirty = true
}

// SetLimits records the device limits of a configuration message.
func (m *Manager) SetLimits(l *ownsandv1.DeviceLimits) {
	if l == nil {
		return
	}
	m.mu.Lock()
	m.limits = l
	m.mu.Unlock()
	if _, err := m.st.UpdateAgent(func(a *store.AgentState) error {
		a.Limits = &store.Limits{VCPUCount: l.GetVcpuCount(), MemoryMiB: l.GetMemoryMib(), DiskGiB: l.GetDiskGib(), VCPUOvercommitPercent: l.GetVcpuOvercommitPercent()}
		return nil
	}); err != nil {
		m.logger.Warn("persist device limits failed", "error", err)
	}
}

// Busy reports whether a sandbox is creating, running or paused, or is
// deleting with a VM stop that has not been confirmed; an update installs
// only when not busy. A DELETING record whose runtime Delete is still running
// or failed (sb.stopped is false) may still have a live VM, so it keeps the
// agent busy until a Delete returns nil or startup reconciliation ends the
// record. Snapshot transfers reserve the device with the updater and are not
// counted here.
func (m *Manager) Busy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, sb := range m.sandboxes {
		switch sb.rec.State {
		case ownsandv1.SandboxState_SANDBOX_STATE_CREATING, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED:
			return true
		case ownsandv1.SandboxState_SANDBOX_STATE_DELETING:
			if !sb.stopped {
				return true
			}
		}
	}
	return false
}

// stricter returns the per-field minimum of two limit sets.
func stricter(a, b *ownsandv1.DeviceLimits) *ownsandv1.DeviceLimits {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	}
	return &ownsandv1.DeviceLimits{
		VcpuCount:             min(a.GetVcpuCount(), b.GetVcpuCount()),
		MemoryMib:             min(a.GetMemoryMib(), b.GetMemoryMib()),
		DiskGib:               min(a.GetDiskGib(), b.GetDiskGib()),
		VcpuOvercommitPercent: min(a.GetVcpuOvercommitPercent(), b.GetVcpuOvercommitPercent()),
	}
}

// checkLimitsLocked checks a create against the stricter of the command's
// limits and the last configuration, as a second check after the
// scheduler.
func (m *Manager) checkLimitsLocked(cmd *ownsandv1.CreateSandboxCommand) error {
	l := stricter(cmd.GetLimits(), m.limits)
	res := cmd.GetResources()
	if l == nil {
		return cmdErr(ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, "device limits are unknown")
	}
	if res.GetVcpuCount() > l.GetVcpuCount() {
		return cmdErr(ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, "sandbox vCPU count exceeds the device vCPU limit")
	}
	var vcpu, mem, disk int64
	macos := 0
	for _, sb := range m.sandboxes {
		if !sb.rec.Live() {
			continue
		}
		vcpu += int64(sb.rec.VCPUCount)
		mem += sb.rec.MemoryMiB
		disk += sb.rec.DiskGiB
		if sb.rec.GuestOS == ownsandv1.GuestOs_GUEST_OS_MACOS {
			macos++
		}
	}
	switch {
	case vcpu+int64(res.GetVcpuCount()) > validate.EffectiveVCPULimit(l):
		return cmdErr(ownsandv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED, "device vCPU limit reached")
	case mem+res.GetMemoryMib() > l.GetMemoryMib():
		return cmdErr(ownsandv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED, "device memory limit reached")
	case disk+res.GetDiskGib() > l.GetDiskGib():
		return cmdErr(ownsandv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED, "device disk limit reached")
	case cmd.GetGuestOs() == ownsandv1.GuestOs_GUEST_OS_MACOS && macos >= MaxMacOSGuests:
		return cmdErr(ownsandv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED, "two macOS guests already run")
	}
	if free, err := m.freeDisk(); err == nil && free < res.GetDiskGib()<<30 {
		return cmdErr(ownsandv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED, "free disk of the state directory does not fit the sandbox disk")
	}
	return nil
}

func validateCreate(cmd *ownsandv1.CreateSandboxCommand) error {
	if err := validate.UUIDv7("sandbox_id", cmd.GetSandboxId()); err != nil {
		return cmdErr(ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
	}
	if err := validate.Resources(cmd.GetResources()); err != nil {
		return cmdErr(ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
	}
	if err := validate.TTL(cmd.GetIdleTtl(), cmd.GetMaxTtl()); err != nil {
		return cmdErr(ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
	}
	if cmd.GetMaxDeadline() == nil || cmd.GetMaxDeadline().CheckValid() != nil {
		return cmdErr(ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "max_deadline is required")
	}
	switch cmd.GetNetworkMode() {
	case ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND, ownsandv1.NetworkMode_NETWORK_MODE_NONE:
	default:
		return cmdErr(ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "network_mode is required")
	}
	if cmd.GetGuestOs() == ownsandv1.GuestOs_GUEST_OS_UNSPECIFIED {
		return cmdErr(ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "guest_os is required")
	}
	if cmd.GetSource() == nil {
		return cmdErr(ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "source is required")
	}
	return nil
}

// existingCreateLocked handles a create for an id that already has a record.
// found is false when the id is unknown. A deleted or terminal record yields a
// failed-precondition error; a live record yields its current entry, like a
// repeated create command. The caller must hold m.mu.
func (m *Manager) existingCreateLocked(id string) (entry *ownsandv1.SandboxStateEntry, err error, found bool) {
	sb, ok := m.sandboxes[id]
	if !ok {
		return nil, nil, false
	}
	entry = sb.rec.Entry(m.epoch)
	if sb.rec.DeleteMarker || sb.rec.Terminal() {
		return entry, cmdErr(ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, "sandbox was deleted before its create"), true
	}
	return entry, nil, true
}

// Create runs a create command. It returns the sandbox entry and, when the
// create was rejected or failed, an error.
func (m *Manager) Create(ctx context.Context, cmd *ownsandv1.CreateSandboxCommand) (*ownsandv1.SandboxStateEntry, error) {
	if err := validateCreate(cmd); err != nil {
		return nil, err
	}
	id := cmd.GetSandboxId()
	m.mu.Lock()
	if entry, err, found := m.existingCreateLocked(id); found {
		m.mu.Unlock()
		return entry, err
	}
	gate := m.gate
	m.mu.Unlock()
	release := func() {}
	if gate != nil {
		r, err := gate.BeginCreate()
		if err != nil {
			return nil, err
		}
		release = r
	}
	defer release()

	m.mu.Lock()
	// The gate may block with m.mu released, so a concurrent delete (marker or
	// terminal record) or a duplicate create may have recorded this id. Repeat
	// the existence check before any limit check, record write or VM start.
	if entry, err, found := m.existingCreateLocked(id); found {
		m.mu.Unlock()
		return entry, err
	}
	if err := m.checkLimitsLocked(cmd); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	spec := vmruntime.Spec{
		SandboxID:            id,
		Dir:                  m.st.Path(store.VMsDir, id),
		Resources:            cmd.GetResources(),
		NetworkMode:          cmd.GetNetworkMode(),
		GuestOS:              cmd.GetGuestOs(),
		MemoryRestoreAllowed: cmd.GetMemoryRestoreAllowed(),
	}
	switch src := cmd.GetSource().(type) {
	case *ownsandv1.CreateSandboxCommand_BaseImage:
		spec.BaseImage = src.BaseImage
	case *ownsandv1.CreateSandboxCommand_SnapshotId:
		snap, ok := m.snapshots[src.SnapshotId]
		if !ok || !snap.usable() {
			m.mu.Unlock()
			return nil, cmdErr(ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, "snapshot is not ready on this device")
		}
		spec.Snapshot = &vmruntime.SnapshotSource{SnapshotID: src.SnapshotId, Dir: snap.dir, HasMemoryState: snap.rec.HasMemoryState, Resources: snap.resources()}
		// The create reads the snapshot content; a snapshot delete waits.
		snap.users++
		defer m.snapshotReleaser(snap)()
	}
	now := m.now()
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sb := &sandbox{createCancel: cancel, rec: Record{
		SandboxID:   id,
		State:       ownsandv1.SandboxState_SANDBOX_STATE_CREATING,
		VCPUCount:   cmd.GetResources().GetVcpuCount(),
		MemoryMiB:   cmd.GetResources().GetMemoryMib(),
		DiskGiB:     cmd.GetResources().GetDiskGib(),
		NetworkMode: cmd.GetNetworkMode(),
		GuestOS:     cmd.GetGuestOs(),
		BaseImage:   spec.BaseImage,
		CreateTime:  now,
		MaxDeadline: cmd.GetMaxDeadline().AsTime(),
		IdleTTL:     cmd.GetIdleTtl().AsDuration(),
	}}
	if spec.Snapshot != nil {
		sb.rec.SnapshotID = spec.Snapshot.SnapshotID
	}
	sb.cmdMu.Lock()
	defer sb.cmdMu.Unlock()
	m.sandboxes[id] = sb
	err := m.commitLocked(sb, func(*Record) {})
	if err != nil {
		// The record was never saved and no VM exists yet. Unpublish the
		// in-memory sandbox so that it does not count against the limits,
		// keep the agent busy or appear in state reports, and so that a
		// retry of the same command can create the sandbox.
		delete(m.sandboxes, id)
	}
	outbox := m.outbox
	entry := sb.rec.Entry(m.epoch)
	m.mu.Unlock()
	if err != nil {
		m.logger.Error("sandbox record failed", "sandbox_id", id, "error", err)
		return nil, cmdErr(ownsandv1.ErrorCode_ERROR_CODE_INTERNAL, "agent could not record the sandbox")
	}
	if outbox != nil {
		outbox.SendSandboxUpdate(entry)
	}
	m.logger.Info("sandbox creating", "sandbox_id", id, "vcpu_count", sb.rec.VCPUCount, "memory_mib", sb.rec.MemoryMiB, "disk_gib", sb.rec.DiskGiB, "network_mode", sb.rec.NetworkMode.String())

	vm, err := m.rt.Create(cctx, spec)
	if err != nil {
		m.logger.Warn("sandbox create failed", "sandbox_id", id, "error", err)
		m.mu.Lock()
		deleting := sb.ending || sb.rec.State != ownsandv1.SandboxState_SANDBOX_STATE_CREATING
		m.mu.Unlock()
		if deleting {
			// A delete or a TTL canceled the create; that path reports the
			// end.
			m.mu.Lock()
			entry := sb.rec.Entry(m.epoch)
			m.mu.Unlock()
			return entry, cmdErr(ownsandv1.ErrorCode_ERROR_CODE_ABORTED, "sandbox was deleted during its create")
		}
		// The runtime cleans up a failed create itself, but a runtime whose
		// stop failed keeps the VM handle and files. Tick retries the stop
		// until the runtime Delete succeeds.
		m.mu.Lock()
		m.cleanupPending[id] = struct{}{}
		m.mu.Unlock()
		entry, uerr := m.update(sb, func(r *Record) {
			r.State = ownsandv1.SandboxState_SANDBOX_STATE_FAILED
			r.EndReason = ownsandv1.SandboxEndReason_SANDBOX_END_REASON_CREATE_FAILED
		})
		if uerr != nil {
			return nil, cmdErr(ownsandv1.ErrorCode_ERROR_CODE_INTERNAL, "agent could not record the failed create")
		}
		code := ownsandv1.ErrorCode_ERROR_CODE_INTERNAL
		if errors.Is(err, vmruntime.ErrUnsupported) {
			code = ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION
		}
		return entry, cmdErr(code, "sandbox VM could not start")
	}
	guest := guestclient.New(vm.Dial)
	m.mu.Lock()
	sb.vm, sb.guest, sb.createCancel = vm, guest, nil
	m.mu.Unlock()
	// Supervise the VM before the fallible running save: when the save fails,
	// the VM still runs, and its later exit must end the record.
	go m.watchVM(sb, vm)
	return m.publishRunning(ctx, sb, vm)
}

// publishRunning records and reports RUNNING for a sandbox whose VM started.
// The caller holds sb.cmdMu. A failed commit is recoverable, because the VM
// runs and a repeated create command would only get the existing creating
// record: it tries the commit up to m.runningAttempts times with a growing
// wait. A revision failure leaves the record unchanged (CREATING), while a
// failed record save leaves the new state in memory and unsaved; the next
// attempt commits either again with a new revision.
//
// A delete or a TTL end that took over (sb.ending, or a record that left
// CREATING and RUNNING) keeps control of the teardown: publishRunning commits
// nothing more and returns ABORTED, and that end stops the VM after the create
// released sb.cmdMu. When the attempts are used up, the create does not report
// success: it ends the sandbox as CREATE_FAILED, which stops the VM. When
// that end fails too, the sandbox stays tracked with its VM, ending cleared,
// so that a TTL tick, a delete or the VM exit ends it later.
func (m *Manager) publishRunning(ctx context.Context, sb *sandbox, vm vmruntime.VM) (*ownsandv1.SandboxStateEntry, error) {
	id := sb.rec.SandboxID
	attempts := max(m.runningAttempts, 1)
	live := func() bool {
		if m.sandboxes[id] != sb || sb.ending {
			return false
		}
		return sb.rec.State == ownsandv1.SandboxState_SANDBOX_STATE_CREATING || sb.rec.State == ownsandv1.SandboxState_SANDBOX_STATE_RUNNING
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		entry, err := m.updateIf(sb, live, func(r *Record) {
			r.State = ownsandv1.SandboxState_SANDBOX_STATE_RUNNING
			r.RestorePath = vm.RestorePath()
			r.IdleDeadline = m.now().Add(r.IdleTTL)
		})
		if err == nil {
			return entry, nil
		}
		if errors.Is(err, errSandboxGone) {
			m.logger.Warn("sandbox ended during its create; the end stops the VM", "sandbox_id", id)
			m.mu.Lock()
			entry := sb.rec.Entry(m.epoch)
			m.mu.Unlock()
			return entry, cmdErr(ownsandv1.ErrorCode_ERROR_CODE_ABORTED, "sandbox was deleted during its create")
		}
		lastErr = err
		// The record state after the error tells the failures apart: a
		// revision failure did not change the record, a save failure did.
		m.mu.Lock()
		stage := "revision"
		if sb.rec.State == ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
			stage = "save"
		}
		m.mu.Unlock()
		willRetry := attempt < attempts && ctx.Err() == nil
		m.logger.Error("sandbox running record failed", "sandbox_id", id, "failed_stage", stage, "attempt", attempt, "max_attempts", attempts, "will_retry", willRetry, "error", err)
		if m.onRunningCommitFailed != nil {
			m.onRunningCommitFailed(attempt, willRetry)
		}
		if !willRetry {
			break
		}
		select {
		case <-time.After(m.runningBackoff * time.Duration(attempt)):
		case <-ctx.Done():
		}
	}
	m.logger.Error("sandbox running record could not be published; ending the sandbox", "sandbox_id", id, "error", lastErr)
	entry, err := m.endLocked(context.Background(), sb, ownsandv1.SandboxState_SANDBOX_STATE_FAILED, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_CREATE_FAILED)
	if err != nil {
		// endLocked cleared sb.ending: the sandbox and its VM stay tracked.
		m.logger.Error("end of the sandbox after a failed running record failed; the sandbox stays tracked for a retry", "sandbox_id", id, "error", err)
		return nil, cmdErr(ownsandv1.ErrorCode_ERROR_CODE_INTERNAL, "agent could not record the sandbox")
	}
	return entry, cmdErr(ownsandv1.ErrorCode_ERROR_CODE_INTERNAL, "agent could not record the sandbox; the sandbox was stopped")
}

// watchVM ends a sandbox whose VM ended without a command.
func (m *Manager) watchVM(sb *sandbox, vm vmruntime.VM) {
	<-vm.Exited()
	m.mu.Lock()
	expected := m.closed || sb.ending || sb.vm != vm
	if !expected {
		m.bg.Add(1)
	}
	m.mu.Unlock()
	if expected {
		return
	}
	defer m.bg.Done()
	sb.cmdMu.Lock()
	defer sb.cmdMu.Unlock()
	m.mu.Lock()
	if m.closed || sb.ending || sb.vm != vm || !sb.rec.Live() {
		m.mu.Unlock()
		return
	}
	reason := sb.rec.endReasonAt(m.now())
	if reason == ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED &&
		sb.rec.State == ownsandv1.SandboxState_SANDBOX_STATE_DELETING &&
		sb.rec.EndReason == ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER {
		// A failed caller delete left the record in DELETING with the caller
		// reason. Without a passed deadline, the VM exit completes that delete
		// and does not turn it into a lost VM, as the retried delete and
		// reconcile also keep the caller reason. Other recorded reasons (a failed
		// create or a lost VM) keep the lost-VM outcome of the exit.
		reason = sb.rec.EndReason
	}
	m.mu.Unlock()
	if reason == ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED {
		reason = ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE
	}
	m.logger.Warn("sandbox VM ended unexpectedly", "sandbox_id", sb.rec.SandboxID)
	if _, err := m.endLocked(context.Background(), sb, finalStateFor(reason), reason); err != nil {
		m.logger.Error("record lost sandbox failed", "sandbox_id", sb.rec.SandboxID, "error", err)
	}
}

// endLocked stops the VM and records the end. The caller holds sb.cmdMu.
// When the attempt fails, it clears sb.ending so that a later Tick or
// Delete can retry the end. The record may stay in DELETING until then. The
// retry runs under sb.cmdMu, so ends still cannot overlap. A retry does not
// repeat the exec tracker notification.
func (m *Manager) endLocked(ctx context.Context, sb *sandbox, state ownsandv1.SandboxState, reason ownsandv1.SandboxEndReason) (entry *ownsandv1.SandboxStateEntry, err error) {
	m.mu.Lock()
	sb.ending = true
	guest := sb.guest
	execs := m.execs
	if sb.rec.Terminal() {
		entry := sb.rec.Entry(m.epoch)
		m.mu.Unlock()
		return entry, nil
	}
	m.mu.Unlock()
	defer func() {
		if err == nil {
			return
		}
		m.mu.Lock()
		if !m.closed && !sb.rec.Terminal() {
			sb.ending = false
		}
		m.mu.Unlock()
	}()
	if _, err := m.update(sb, func(r *Record) {
		r.State = ownsandv1.SandboxState_SANDBOX_STATE_DELETING
		r.EndReason = reason
	}); err != nil {
		return nil, err
	}
	m.mu.Lock()
	stopped := sb.stopped
	m.mu.Unlock()
	if !stopped {
		if err := m.rt.Delete(ctx, sb.rec.SandboxID); err != nil {
			m.logger.Error("runtime delete failed", "sandbox_id", sb.rec.SandboxID, "error", err)
			return nil, cmdErr(ownsandv1.ErrorCode_ERROR_CODE_INTERNAL, "sandbox VM could not be stopped")
		}
		if execs != nil {
			execs.SandboxEnded(sb.rec.SandboxID, reason)
		}
		if guest != nil {
			guest.Close()
		}
		m.mu.Lock()
		sb.vm, sb.guest, sb.stopped = nil, nil, true
		m.mu.Unlock()
	}
	return m.update(sb, func(r *Record) {
		r.State = state
		r.EndReason = reason
	})
}

// Delete runs a delete command. It is idempotent; a delete before the
// create leaves a delete marker.
func (m *Manager) Delete(ctx context.Context, cmd *ownsandv1.DeleteSandboxCommand) (*ownsandv1.SandboxStateEntry, error) {
	id := cmd.GetSandboxId()
	if err := validate.UUIDv7("sandbox_id", id); err != nil {
		return nil, cmdErr(ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
	}
	reason := cmd.GetEndReason()
	if reason == ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED {
		reason = ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER
	}
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	if !ok {
		sb = &sandbox{rec: Record{SandboxID: id, State: ownsandv1.SandboxState_SANDBOX_STATE_DELETED, EndReason: reason, DeleteMarker: true, CreateTime: m.now()}}
		// Publish and save the marker in one m.mu section. A marker that
		// is only in memory must never be visible: a retried delete would
		// find it terminal and report success although a restart would lose
		// it, and a delayed create would then start the sandbox.
		m.sandboxes[id] = sb
		err := m.commitLocked(sb, func(*Record) {})
		if err != nil {
			// Unpublish the unsaved marker, as Create does for its
			// record, so that a retry of the delete saves it again.
			delete(m.sandboxes, id)
		}
		entry := sb.rec.Entry(m.epoch)
		outbox := m.outbox
		m.mu.Unlock()
		if err != nil {
			m.logger.Error("delete marker failed", "sandbox_id", id, "error", err)
			return nil, cmdErr(ownsandv1.ErrorCode_ERROR_CODE_INTERNAL, "agent could not record the delete marker")
		}
		m.logger.Info("delete before create; leaving a delete marker", "sandbox_id", id)
		if outbox != nil {
			outbox.SendSandboxUpdate(entry)
		}
		return entry, nil
	}
	if cancel := sb.createCancel; cancel != nil {
		sb.ending = true
		cancel()
	}
	m.mu.Unlock()
	sb.cmdMu.Lock()
	defer sb.cmdMu.Unlock()
	m.mu.Lock()
	terminal := sb.rec.Terminal()
	entry := sb.rec.Entry(m.epoch)
	m.mu.Unlock()
	if terminal {
		return entry, nil
	}
	return m.endLocked(ctx, sb, ownsandv1.SandboxState_SANDBOX_STATE_DELETED, reason)
}

// Pause runs a pause command.
func (m *Manager) Pause(ctx context.Context, cmd *ownsandv1.PauseSandboxCommand) (*ownsandv1.SandboxStateEntry, error) {
	return m.pauseResume(ctx, cmd.GetSandboxId(), true)
}

// Resume runs a resume command. After the resume it sets the guest clock.
func (m *Manager) Resume(ctx context.Context, cmd *ownsandv1.ResumeSandboxCommand) (*ownsandv1.SandboxStateEntry, error) {
	return m.pauseResume(ctx, cmd.GetSandboxId(), false)
}

func (m *Manager) pauseResume(ctx context.Context, id string, pause bool) (*ownsandv1.SandboxStateEntry, error) {
	sb, ok := m.get(id)
	if !ok {
		return nil, cmdErr(ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND, "sandbox is unknown on this device")
	}
	sb.cmdMu.Lock()
	defer sb.cmdMu.Unlock()
	m.mu.Lock()
	state, vm, guest, entry := sb.rec.State, sb.vm, sb.guest, sb.rec.Entry(m.epoch)
	m.mu.Unlock()
	from, to := ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED
	if !pause {
		from, to = to, from
	}
	switch state {
	case to:
		return entry, nil
	case from:
	default:
		return entry, cmdErr(ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, "sandbox is %s", state)
	}
	if vm == nil {
		// A record kept live for a pending restart cleanup has no VM.
		return entry, cmdErr(ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, "sandbox VM is not available")
	}
	if pause {
		if err := m.rt.Pause(ctx, vm); err != nil {
			m.logger.Error("pause failed", "sandbox_id", id, "error", err)
			return entry, cmdErr(ownsandv1.ErrorCode_ERROR_CODE_INTERNAL, "sandbox could not be paused")
		}
		updated, err := m.update(sb, func(r *Record) { r.State = to })
		if err != nil {
			err = m.undoRuntimeMove(sb, id, from, err)
		}
		return updated, err
	}
	if err := m.rt.Resume(ctx, vm); err != nil {
		m.logger.Error("resume failed", "sandbox_id", id, "error", err)
		return entry, cmdErr(ownsandv1.ErrorCode_ERROR_CODE_INTERNAL, "sandbox could not be resumed")
	}
	clockCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	if err := guest.SetClock(clockCtx, m.now()); err != nil {
		m.logger.Warn("set guest clock after resume failed", "sandbox_id", id, "error", err)
	}
	cancel()
	updated, err := m.update(sb, func(r *Record) {
		r.State = to
		r.IdleDeadline = m.now().Add(r.IdleTTL)
	})
	if err != nil {
		err = m.undoRuntimeMove(sb, id, from, err)
	}
	return updated, err
}

// undoRuntimeMove runs after a pause or resume that the runtime accepted but
// whose record update failed. When the record is still in state from, nextRevision
// failed before the change applied, so the VM is in the opposite state from
// the record: it moves the VM back, because a later opposite command would
// otherwise see the old state as its target, return success and never reach
// the runtime. When the record already holds the new state, SaveSandbox failed
// after the change applied; the VM matches the record and a retry returns the
// current entry, so nothing is undone. It returns updateErr, joined with the
// error of the undo when the undo fails.
//
// The undo calls the runtime directly and uses a fresh bounded context: the
// request context can already be canceled, and the undo must not depend on it.
func (m *Manager) undoRuntimeMove(sb *sandbox, id string, from ownsandv1.SandboxState, updateErr error) error {
	m.mu.Lock()
	state, vm := sb.rec.State, sb.vm
	m.mu.Unlock()
	if state != from {
		return updateErr
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var err error
	if from == ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		// The runtime paused the VM; the record is still RUNNING.
		err = m.rt.Resume(ctx, vm)
	} else {
		err = m.rt.Pause(ctx, vm)
	}
	if err != nil {
		m.logger.Error("undo of the runtime move after a failed record update failed; the VM no longer matches the record", "sandbox_id", id, "state", from.String(), "error", err)
		return errors.Join(updateErr, err)
	}
	m.logger.Warn("undid the runtime move after a failed record update", "sandbox_id", id, "state", from.String(), "error", updateErr)
	return updateErr
}

// retryEndBeforeDeadline reports whether Tick can retry a failed end before
// a TTL deadline. The caller holds m.mu.
func (sb *sandbox) retryEndBeforeDeadline() bool {
	if sb.rec.State != ownsandv1.SandboxState_SANDBOX_STATE_DELETING {
		return false
	}
	switch sb.rec.EndReason {
	case ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE:
		return true
	case ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER:
		if sb.vm != nil {
			// The watcher may have seen the VM exit while Delete still owned the end.
			select {
			case <-sb.vm.Exited():
				return true
			default:
			}
		}
	}
	return false
}

// Tick enforces the TTLs, retries cleanup of exited VMs, reports moved idle
// deadlines at most once per minute and resends unacknowledged updates when
// resend is true.
func (m *Manager) Tick(ctx context.Context, resend bool) {
	now := m.now()
	type expiry struct {
		sb     *sandbox
		reason ownsandv1.SandboxEndReason
		// retry marks cleanup that is due without a passed deadline.
		retry bool
	}
	var expired []expiry
	// idleReport is one pending idle-deadline report. previous is the
	// lastIdleReport value before the report consumed the pending flag, so a
	// failed report can put both back.
	type idleReport struct {
		sb       *sandbox
		previous time.Time
	}
	var reports []idleReport
	var unacked []*ownsandv1.SandboxStateEntry
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	execs := m.execs
	for _, sb := range m.sandboxes {
		if sb.ending {
			// endLocked leaves ending set after a successful end, so a
			// terminal record that the control plane has not acknowledged
			// still needs its resend. A record whose end is in progress is
			// not terminal yet and stays skipped.
			if resend && sb.rec.Terminal() && !sb.rec.Acked {
				unacked = append(unacked, sb.rec.Entry(m.epoch))
			}
			continue
		}
		if sb.rec.State == ownsandv1.SandboxState_SANDBOX_STATE_RUNNING && execs != nil && !now.Before(sb.rec.IdleDeadline) && execs.HasRunningExec(sb.rec.SandboxID) {
			// A running exec keeps the sandbox active.
			sb.rec.IdleDeadline = now.Add(sb.rec.IdleTTL)
			sb.idleDirty = true
		}
		if sb.rec.Live() {
			if reason := sb.rec.endReasonAt(now); reason != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED {
				sb.ending = true
				if sb.createCancel != nil {
					sb.createCancel()
				}
				expired = append(expired, expiry{sb: sb, reason: reason})
				continue
			}
			if sb.retryEndBeforeDeadline() {
				sb.ending = true
				expired = append(expired, expiry{sb: sb, reason: sb.rec.EndReason, retry: true})
				continue
			}
		}
		if sb.idleDirty && now.Sub(sb.lastIdleReport) >= IdleReportInterval {
			reports = append(reports, idleReport{sb: sb, previous: sb.lastIdleReport})
			sb.idleDirty = false
			sb.lastIdleReport = now
		}
		if resend && !sb.rec.Acked {
			unacked = append(unacked, sb.rec.Entry(m.epoch))
		}
	}
	// Collect the failed creates whose runtime clean-up still has to be
	// confirmed. A sandbox with a live record owns its VM: its own end calls
	// the runtime Delete, so the retry waits until that record is terminal
	// or dropped.
	var cleanups []string
	for id := range m.cleanupPending {
		if _, running := m.cleanupInFlight[id]; running {
			continue
		}
		if sb, ok := m.sandboxes[id]; ok && sb.rec.Live() {
			continue
		}
		m.cleanupInFlight[id] = struct{}{}
		cleanups = append(cleanups, id)
	}
	outbox := m.outbox
	m.bg.Add(len(expired) + len(cleanups))
	m.mu.Unlock()
	for _, id := range cleanups {
		go m.retryCreateCleanup(ctx, id)
	}
	for _, e := range expired {
		if e.retry {
			m.logger.Info("retrying sandbox cleanup", "sandbox_id", e.sb.rec.SandboxID, "end_reason", e.reason.String())
		} else {
			m.logger.Info("sandbox TTL expired", "sandbox_id", e.sb.rec.SandboxID, "end_reason", e.reason.String())
		}
		go func(e expiry) {
			defer m.bg.Done()
			e.sb.cmdMu.Lock()
			defer e.sb.cmdMu.Unlock()
			if _, err := m.endLocked(ctx, e.sb, finalStateFor(e.reason), e.reason); err != nil {
				m.logger.Error("end sandbox failed", "sandbox_id", e.sb.rec.SandboxID, "error", err)
			}
		}(e)
	}
	if m.beforeIdleReports != nil {
		m.beforeIdleReports()
	}
	for _, r := range reports {
		m.reportIdle(r.sb, r.previous)
	}
	if outbox != nil {
		for _, e := range unacked {
			outbox.SendSandboxUpdate(e)
		}
	}
}

// retryCreateCleanup calls the runtime Delete for a failed create. The runtime
// Delete is idempotent: it stops a VM that the failed create kept and removes
// its files, and it succeeds when nothing is left. A success releases the ID;
// a failure keeps it for the next Tick. The caller registered the call in
// m.bg and m.cleanupInFlight.
func (m *Manager) retryCreateCleanup(ctx context.Context, id string) {
	defer m.bg.Done()
	err := m.rt.Delete(ctx, id)
	m.mu.Lock()
	delete(m.cleanupInFlight, id)
	if err == nil {
		delete(m.cleanupPending, id)
	}
	m.mu.Unlock()
	if err != nil {
		m.logger.Warn("clean-up of a failed create incomplete; the next tick retries it", "sandbox_id", id, "error", err)
		return
	}
	m.logger.Info("clean-up of a failed create complete", "sandbox_id", id)
}

// idleReportableLocked reports whether sb is still the live, registered
// sandbox that can publish an idle-deadline report. A sandbox that an
// acknowledgement dropped, or that is ending or terminal, must not commit a
// new revision: that would recreate its deleted record file. The caller holds
// m.mu.
func (m *Manager) idleReportableLocked(sb *sandbox) bool {
	return m.sandboxes[sb.rec.SandboxID] == sb && !sb.ending && sb.rec.Live()
}

// reportIdle publishes the moved idle deadline of sb with a new revision. Tick
// consumed the pending report (idleDirty and the throttle timestamp) when it
// collected sb; previous is the timestamp from before that. When the revision
// cannot be persisted, reportIdle restores both so that a later tick retries
// without another activity event; the one-minute throttle applies only after
// a successful report. A sandbox that stopped being reportable since the
// collection is skipped.
func (m *Manager) reportIdle(sb *sandbox, previous time.Time) {
	id := sb.rec.SandboxID
	_, err := m.updateIf(sb, func() bool { return m.idleReportableLocked(sb) }, func(*Record) {})
	if err == nil {
		return
	}
	if errors.Is(err, errSandboxGone) {
		m.logger.Debug("skipped idle deadline report of a sandbox that is no longer live", "sandbox_id", id)
		return
	}
	m.mu.Lock()
	if m.idleReportableLocked(sb) {
		sb.idleDirty = true
		sb.lastIdleReport = previous
	}
	m.mu.Unlock()
	m.logger.Warn("report idle deadline failed; retrying on a later tick", "sandbox_id", id, "error", err)
}

// Run calls Tick every second and resends unacknowledged updates every
// ResendInterval until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	lastResend := m.now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			resend := m.now().Sub(lastResend) >= ResendInterval
			if resend {
				lastResend = m.now()
			}
			m.Tick(ctx, resend)
		}
	}
}

// StateReport returns the full state of the device for a new channel.
func (m *Manager) StateReport(seq uint64, builtImages []ownsandv1.BaseImage) *ownsandv1.StateReport {
	m.mu.Lock()
	defer m.mu.Unlock()
	report := &ownsandv1.StateReport{ReportSequence: seq, StateEpoch: m.epoch, BuiltBaseImages: builtImages}
	revs := map[string]uint64{}
	ids := make([]string, 0, len(m.sandboxes))
	for id := range m.sandboxes {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		sb := m.sandboxes[id]
		report.Sandboxes = append(report.Sandboxes, sb.rec.Entry(m.epoch))
		revs[id] = sb.rec.Revision
	}
	for _, snap := range m.snapshots {
		report.Snapshots = append(report.Snapshots, snap.entry())
	}
	m.reports[seq] = revs
	// Keep only the last few unacknowledged reports.
	for s := range m.reports {
		if s+8 < seq {
			delete(m.reports, s)
		}
	}
	return report
}

// AckStateReport applies a state report acknowledgement: it marks the
// acknowledged revisions and drops the records of ended sandboxes that the
// report contained.
func (m *Manager) AckStateReport(ack *ownsandv1.StateReportAck) {
	m.mu.Lock()
	revs := m.reports[ack.GetReportSequence()]
	delete(m.reports, ack.GetReportSequence())
	m.mu.Unlock()
	for _, a := range ack.GetSandboxes() {
		m.AckSandboxUpdate(a)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, rev := range revs {
		sb, ok := m.sandboxes[id]
		if ok && sb.rec.Terminal() && sb.rec.Revision == rev {
			m.dropLocked(id)
		}
	}
}

// AckSandboxUpdate applies a sandbox update acknowledgement. An
// acknowledged terminal record is dropped.
func (m *Manager) AckSandboxUpdate(ack *ownsandv1.SandboxUpdateAck) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sb, ok := m.sandboxes[ack.GetSandboxId()]
	if !ok || ack.GetRevision().GetEpoch() != m.epoch || ack.GetRevision().GetCounter() < sb.rec.Revision {
		return
	}
	sb.rec.Acked = true
	if sb.rec.Terminal() {
		m.dropLocked(ack.GetSandboxId())
		return
	}
	if err := m.st.SaveSandbox(sb.rec.SandboxID, sb.rec); err != nil {
		m.logger.Warn("save acknowledged record failed", "sandbox_id", sb.rec.SandboxID, "error", err)
	}
}

func (m *Manager) dropLocked(id string) {
	sb := m.sandboxes[id]
	if sb != nil && sb.rec.DeleteMarker {
		// Keep the delete marker so that a late create is still rejected;
		// the record stays acknowledged.
		sb.rec.Acked = true
		_ = m.st.SaveSandbox(id, sb.rec)
		return
	}
	delete(m.sandboxes, id)
	if err := m.st.DeleteSandbox(id); err != nil {
		m.logger.Warn("delete sandbox record failed", "sandbox_id", id, "error", err)
	}
}

// Entries returns the entries of every sandbox, for status output.
func (m *Manager) Entries() []*ownsandv1.SandboxStateEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*ownsandv1.SandboxStateEntry
	for _, sb := range m.sandboxes {
		out = append(out, sb.rec.Entry(m.epoch))
	}
	return out
}

// RemoveAll stops and deletes every sandbox and snapshot for a device
// removal. Records are deleted without updates: the device is removed. When
// the runtime fails to delete a sandbox, RemoveAll keeps that sandbox
// tracked, leaves the local data on disk and returns the error, so that a
// retry deletes the sandbox again; a retry after a complete clean-up is a
// no-op.
func (m *Manager) RemoveAll(ctx context.Context) error {
	if err := m.fenceSnapshotImports(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	sbs := make([]*sandbox, 0, len(m.sandboxes))
	for _, sb := range m.sandboxes {
		sbs = append(sbs, sb)
		if sb.createCancel != nil {
			sb.createCancel()
		}
	}
	m.mu.Unlock()
	var errs []error
	deleted := make(map[string]bool, len(sbs))
	for _, sb := range sbs {
		sb.cmdMu.Lock()
		m.mu.Lock()
		sb.ending = true
		m.mu.Unlock()
		if err := m.rt.Delete(ctx, sb.rec.SandboxID); err != nil {
			errs = append(errs, err)
		} else {
			deleted[sb.rec.SandboxID] = true
		}
		m.mu.Lock()
		execs := m.execs
		m.mu.Unlock()
		if execs != nil {
			execs.SandboxEnded(sb.rec.SandboxID, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DEVICE_REMOVED)
		}
		sb.cmdMu.Unlock()
	}
	// Forget only the sandboxes that the runtime deleted. A sandbox whose
	// deletion failed stays tracked, together with its record and VM files
	// on disk, so that the retry of the removal deletes it again instead of
	// leaving a running VM that no record refers to.
	m.mu.Lock()
	for id := range deleted {
		delete(m.sandboxes, id)
	}
	if len(errs) == 0 {
		m.snapshots = map[string]*snapshot{}
	}
	m.mu.Unlock()
	if len(errs) > 0 {
		// Do not wipe the local data: the wipe would remove the records
		// and VM files of the failed sandboxes. The wipe is idempotent, so
		// the retry that finds no failure runs it.
		return errors.Join(errs...)
	}
	if err := m.st.WipeLocalData(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// removeDirIfExists removes a directory tree.
func removeDirIfExists(dir string) error {
	if err := fsutil.RemoveAll(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
