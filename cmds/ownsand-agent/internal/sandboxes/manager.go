// SPDX-License-Identifier: Apache-2.0

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
	// pending holds the snapshots whose capture runs now, by ID. They are
	// not in snapshots, so they are not usable and not reported.
	pending   map[string]*pendingSnapshot
	limits    *ownsandv1.DeviceLimits
	transfers int
	reports   map[uint64]map[string]uint64
	outbox    Outbox
	execs     ExecTracker
	gate      CreateGate
	closed    bool
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
		sandboxes: map[string]*sandbox{}, snapshots: map[string]*snapshot{}, pending: map[string]*pendingSnapshot{}, reports: map[uint64]map[string]uint64{},
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
// that survived (a failed binding) are stopped.
func (m *Manager) reconcile(ctx context.Context) error {
	running, err := m.rt.Running(ctx)
	if err != nil {
		return fmt.Errorf("sandboxes: list runtime VMs: %w", err)
	}
	for _, id := range running {
		m.logger.Warn("stopping a VM that outlived its agent process", "sandbox_id", id)
		if err := m.rt.Delete(ctx, id); err != nil {
			m.logger.Error("stop surviving VM failed", "sandbox_id", id, "error", err)
		}
	}
	now := m.now()
	for _, sb := range m.sandboxes {
		if !sb.rec.Live() {
			continue
		}
		reason := sb.rec.ttlReason(now)
		state := ownsandv1.SandboxState_SANDBOX_STATE_DELETED
		switch {
		case reason != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED:
		case sb.rec.State == ownsandv1.SandboxState_SANDBOX_STATE_DELETING:
			reason = sb.rec.EndReason
		default:
			state = ownsandv1.SandboxState_SANDBOX_STATE_FAILED
			reason = ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE
		}
		m.logger.Info("sandbox ended while the agent was stopped", "sandbox_id", sb.rec.SandboxID, "state", state.String(), "end_reason", reason.String())
		if err := m.commitLocked(sb, func(r *Record) {
			r.State = state
			r.EndReason = reason
		}); err != nil {
			return err
		}
	}
	return nil
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

// update commits a change and sends the sandbox update.
func (m *Manager) update(sb *sandbox, change func(*Record)) (*ownsandv1.SandboxStateEntry, error) {
	m.mu.Lock()
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

// Busy reports whether a sandbox is creating, running or paused, or a
// snapshot export or import runs; an update installs only when not busy.
func (m *Manager) Busy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.transfers > 0 {
		return true
	}
	for _, sb := range m.sandboxes {
		switch sb.rec.State {
		case ownsandv1.SandboxState_SANDBOX_STATE_CREATING, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED:
			return true
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
	outbox := m.outbox
	entry := sb.rec.Entry(m.epoch)
	m.mu.Unlock()
	if err != nil {
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
	entry, err = m.update(sb, func(r *Record) {
		r.State = ownsandv1.SandboxState_SANDBOX_STATE_RUNNING
		r.RestorePath = vm.RestorePath()
		r.IdleDeadline = m.now().Add(r.IdleTTL)
	})
	if err != nil {
		return nil, cmdErr(ownsandv1.ErrorCode_ERROR_CODE_INTERNAL, "agent could not record the sandbox")
	}
	go m.watchVM(sb, vm)
	return entry, nil
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
	reason := sb.rec.ttlReason(m.now())
	m.mu.Unlock()
	state := ownsandv1.SandboxState_SANDBOX_STATE_DELETED
	if reason == ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED {
		state, reason = ownsandv1.SandboxState_SANDBOX_STATE_FAILED, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE
	}
	m.logger.Warn("sandbox VM ended unexpectedly", "sandbox_id", sb.rec.SandboxID)
	if _, err := m.endLocked(context.Background(), sb, state, reason); err != nil {
		m.logger.Error("record lost sandbox failed", "sandbox_id", sb.rec.SandboxID, "error", err)
	}
}

// endLocked stops the VM and records the end. The caller holds sb.cmdMu.
// When the attempt fails, it clears sb.ending so that a later Tick (TTL) or
// Delete retries the end; the record may stay in DELETING until then. The
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
		m.sandboxes[id] = sb
		m.mu.Unlock()
		m.logger.Info("delete before create; leaving a delete marker", "sandbox_id", id)
		return m.update(sb, func(*Record) {})
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
	if pause {
		if err := m.rt.Pause(ctx, vm); err != nil {
			m.logger.Error("pause failed", "sandbox_id", id, "error", err)
			return entry, cmdErr(ownsandv1.ErrorCode_ERROR_CODE_INTERNAL, "sandbox could not be paused")
		}
		return m.update(sb, func(r *Record) { r.State = to })
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
	return m.update(sb, func(r *Record) {
		r.State = to
		r.IdleDeadline = m.now().Add(r.IdleTTL)
	})
}

// Tick enforces the TTLs, reports moved idle deadlines at most once per
// minute and resends unacknowledged updates when resend is true.
func (m *Manager) Tick(ctx context.Context, resend bool) {
	now := m.now()
	type expiry struct {
		sb     *sandbox
		reason ownsandv1.SandboxEndReason
	}
	var expired []expiry
	var reports []*sandbox
	var unacked []*ownsandv1.SandboxStateEntry
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	execs := m.execs
	for _, sb := range m.sandboxes {
		if sb.ending {
			continue
		}
		if sb.rec.State == ownsandv1.SandboxState_SANDBOX_STATE_RUNNING && execs != nil && !now.Before(sb.rec.IdleDeadline) && execs.HasRunningExec(sb.rec.SandboxID) {
			// A running exec keeps the sandbox active.
			sb.rec.IdleDeadline = now.Add(sb.rec.IdleTTL)
			sb.idleDirty = true
		}
		if sb.rec.Live() {
			if reason := sb.rec.ttlReason(now); reason != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED {
				sb.ending = true
				if sb.createCancel != nil {
					sb.createCancel()
				}
				expired = append(expired, expiry{sb, reason})
				continue
			}
		}
		if sb.idleDirty && now.Sub(sb.lastIdleReport) >= IdleReportInterval {
			sb.idleDirty = false
			sb.lastIdleReport = now
			reports = append(reports, sb)
		}
		if resend && !sb.rec.Acked {
			unacked = append(unacked, sb.rec.Entry(m.epoch))
		}
	}
	outbox := m.outbox
	m.bg.Add(len(expired))
	m.mu.Unlock()
	for _, e := range expired {
		m.logger.Info("sandbox TTL expired", "sandbox_id", e.sb.rec.SandboxID, "end_reason", e.reason.String())
		go func(e expiry) {
			defer m.bg.Done()
			e.sb.cmdMu.Lock()
			defer e.sb.cmdMu.Unlock()
			if _, err := m.endLocked(ctx, e.sb, ownsandv1.SandboxState_SANDBOX_STATE_DELETED, e.reason); err != nil {
				m.logger.Error("end expired sandbox failed", "sandbox_id", e.sb.rec.SandboxID, "error", err)
			}
		}(e)
	}
	for _, sb := range reports {
		if _, err := m.update(sb, func(*Record) {}); err != nil {
			m.logger.Warn("report idle deadline failed", "sandbox_id", sb.rec.SandboxID, "error", err)
		}
	}
	if outbox != nil {
		for _, e := range unacked {
			outbox.SendSandboxUpdate(e)
		}
	}
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
// removal. Records are deleted without updates: the device is removed.
func (m *Manager) RemoveAll(ctx context.Context) error {
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
	for _, sb := range sbs {
		sb.cmdMu.Lock()
		m.mu.Lock()
		sb.ending = true
		m.mu.Unlock()
		if err := m.rt.Delete(ctx, sb.rec.SandboxID); err != nil {
			errs = append(errs, err)
		}
		m.mu.Lock()
		execs := m.execs
		m.mu.Unlock()
		if execs != nil {
			execs.SandboxEnded(sb.rec.SandboxID, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DEVICE_REMOVED)
		}
		sb.cmdMu.Unlock()
	}
	m.mu.Lock()
	m.sandboxes = map[string]*sandbox{}
	m.snapshots = map[string]*snapshot{}
	m.mu.Unlock()
	if err := m.st.WipeLocalData(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// removeDirIfExists removes a directory tree.
func removeDirIfExists(dir string) error {
	if err := os.RemoveAll(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
