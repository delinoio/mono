package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/guestconn"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/runtime"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/state"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// guestOSOf returns the guest OS of a base image.
func guestOSOf(b ownsandv1.BaseImage) ownsandv1.GuestOs {
	switch b {
	case ownsandv1.BaseImage_BASE_IMAGE_LINUX:
		return ownsandv1.GuestOs_GUEST_OS_LINUX
	case ownsandv1.BaseImage_BASE_IMAGE_MACOS:
		return ownsandv1.GuestOs_GUEST_OS_MACOS
	case ownsandv1.BaseImage_BASE_IMAGE_WINDOWS:
		return ownsandv1.GuestOs_GUEST_OS_WINDOWS
	default:
		return ownsandv1.GuestOs_GUEST_OS_UNSPECIFIED
	}
}

// effectiveLimits returns the stricter of the command limits and the last
// configuration limits in each dimension.
func effectiveLimits(a, b *ownsandv1.DeviceLimits) *ownsandv1.DeviceLimits {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	return &ownsandv1.DeviceLimits{
		VcpuCount: min(a.GetVcpuCount(), b.GetVcpuCount()),
		MemoryMib: min(a.GetMemoryMib(), b.GetMemoryMib()),
		DiskGib:   min(a.GetDiskGib(), b.GetDiskGib()),
	}
}

// checkLimitsLocked checks a create against the device limits: a request
// above the limits themselves fails with failed_precondition, and a request
// above the free capacity fails with resource_exhausted.
func (m *Manager) checkLimitsLocked(c *ownsandv1.CreateSandboxCommand, guestOS ownsandv1.GuestOs) error {
	lim := effectiveLimits(c.GetLimits(), m.limits)
	if lim == nil {
		return failedPrecondition("the device has no limits")
	}
	res := c.GetResources()
	if res.GetVcpuCount() > lim.GetVcpuCount() || res.GetMemoryMib() > lim.GetMemoryMib() || res.GetDiskGib() > lim.GetDiskGib() {
		return failedPrecondition("the request exceeds the device limits")
	}
	var cpu, mem, disk int64
	macos := 0
	for _, r := range m.recs.Sandboxes {
		if r.Terminal() || r.DeleteMarker {
			continue
		}
		cpu += r.VCPUs
		mem += r.MemoryMiB
		disk += r.DiskGiB
		if r.GuestOS == ownsandv1.GuestOs_GUEST_OS_MACOS {
			macos++
		}
	}
	if cpu+res.GetVcpuCount() > lim.GetVcpuCount() || mem+res.GetMemoryMib() > lim.GetMemoryMib() || disk+res.GetDiskGib() > lim.GetDiskGib() {
		return connect.NewError(connect.CodeResourceExhausted, errors.New("the device has no free capacity for the request"))
	}
	if guestOS == ownsandv1.GuestOs_GUEST_OS_MACOS && macos >= MaxMacOSGuests {
		return connect.NewError(connect.CodeResourceExhausted, errors.New("the device already runs two macOS guests"))
	}
	if free := m.cfg.FreeDisk(); free < res.GetDiskGib()<<30 {
		return connect.NewError(connect.CodeResourceExhausted, errors.New("the free disk does not fit the requested disk"))
	}
	return nil
}

// create handles a create command on the sandbox's queue.
func (m *Manager) create(commandID string, c *ownsandv1.CreateSandboxCommand) *ownsandv1.AgentCommandResult {
	sid := c.GetSandboxId()
	log := m.log.With("sandbox_id", sid, "command_id", commandID)
	done, err := m.cfg.Gate.BeginCreate()
	if err != nil {
		log.Info("create rejected by the update state")
		return Result(commandID, err)
	}
	defer done()

	spec, rec, err := m.admitCreate(c)
	if err != nil {
		if rec != nil {
			// A retry of a create that the agent already accepted returns
			// the current state.
			return m.withReport(commandID, sid, nil)
		}
		log.Info("create rejected", "code", connect.CodeOf(err).String())
		return Result(commandID, err)
	}
	ctx, cancel := context.WithCancel(m.ctx)
	defer cancel()
	m.mu.Lock()
	m.creates[sid] = cancel
	m.changeLocked(rec, func(*state.SandboxRecord) {})
	m.mu.Unlock()

	path, err := m.boot(ctx, spec)
	m.mu.Lock()
	delete(m.creates, sid)
	m.mu.Unlock()
	if err != nil {
		_ = m.cfg.Runtime.Delete(context.WithoutCancel(ctx), sid)
		m.dropGuest(sid)
		if ctx.Err() != nil {
			// A delete cancelled the create. The delete job that follows on
			// the queue reports the sandbox as deleted.
			log.Info("create cancelled by a delete")
			return Result(commandID, connect.NewError(connect.CodeAborted, errors.New("the create was cancelled by a delete")))
		}
		log.Error("create failed", "error", err)
		m.mu.Lock()
		m.changeLocked(rec, func(r *state.SandboxRecord) {
			r.State = ownsandv1.SandboxState_SANDBOX_STATE_FAILED
			r.EndReason = ownsandv1.SandboxEndReason_SANDBOX_END_REASON_CREATE_FAILED
		})
		m.mu.Unlock()
		return m.withReport(commandID, sid, nil)
	}
	m.mu.Lock()
	m.changeLocked(rec, func(r *state.SandboxRecord) {
		r.State = ownsandv1.SandboxState_SANDBOX_STATE_RUNNING
		r.RestorePath = path
	})
	m.mu.Unlock()
	log.Info("sandbox created", "restore_path", path.String())
	return m.withReport(commandID, sid, nil)
}

// admitCreate checks a create and returns its VM spec and new record. For a
// sandbox that already has a record it returns that record and an error.
func (m *Manager) admitCreate(c *ownsandv1.CreateSandboxCommand) (runtime.VMSpec, *state.SandboxRecord, error) {
	sid := c.GetSandboxId()
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.recs.Sandboxes[sid]; r != nil {
		if r.DeleteMarker {
			return runtime.VMSpec{}, nil, failedPrecondition("the sandbox was deleted before its create arrived")
		}
		return runtime.VMSpec{}, r, errors.New("sandbox already exists")
	}
	res := c.GetResources()
	if sid == "" || res.GetVcpuCount() <= 0 || res.GetMemoryMib() <= 0 || res.GetDiskGib() <= 0 {
		return runtime.VMSpec{}, nil, connect.NewError(connect.CodeInvalidArgument, errors.New("sandbox ID and resources are required"))
	}
	if c.GetMaxDeadline() == nil || c.GetIdleDeadline() == nil || c.GetIdleTtl() == nil {
		return runtime.VMSpec{}, nil, connect.NewError(connect.CodeInvalidArgument, errors.New("TTL deadlines are required"))
	}
	if c.GetImport() != nil {
		return runtime.VMSpec{}, nil, connect.NewError(connect.CodeUnimplemented, errors.New("snapshot import is not implemented by this agent"))
	}
	network := c.GetNetworkMode()
	if network == ownsandv1.NetworkMode_NETWORK_MODE_UNSPECIFIED {
		network = ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND
	}
	spec := runtime.VMSpec{
		SandboxID: sid,
		VCPUs:     res.GetVcpuCount(),
		MemoryMiB: res.GetMemoryMib(),
		DiskGiB:   res.GetDiskGib(),
		Network:   network,
	}
	caps := m.cfg.Runtime.Capabilities(m.ctx)
	switch src := c.GetSource().GetSource().(type) {
	case *ownsandv1.SandboxSource_BaseImage:
		spec.Source.BaseImage = src.BaseImage
		spec.GuestOS = guestOSOf(src.BaseImage)
		if !contains(caps.GuestOSes, spec.GuestOS) {
			return runtime.VMSpec{}, nil, failedPrecondition("the device does not support the guest OS")
		}
		if !contains(caps.BuiltBaseImages, src.BaseImage) {
			return runtime.VMSpec{}, nil, failedPrecondition("the base image is not built on the device")
		}
	case *ownsandv1.SandboxSource_SnapshotId:
		snap := m.recs.Snapshots[src.SnapshotId]
		if snap == nil || snap.State != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY {
			return runtime.VMSpec{}, nil, failedPrecondition("the snapshot is not ready on the device")
		}
		if spec.DiskGiB < snap.DiskGiB {
			return runtime.VMSpec{}, nil, connect.NewError(connect.CodeInvalidArgument, errors.New("disk_gib is smaller than the snapshot disk"))
		}
		spec.Source.SnapshotID = snap.SnapshotID
		spec.GuestOS = snap.GuestOS
		// Memory state restores only with the snapshot's vCPU count, memory
		// and network mode, because the saved state includes the devices.
		spec.TryMemoryRestore = c.GetTryMemoryRestore() && snap.HasMemoryState &&
			snap.VCPUs == spec.VCPUs && snap.MemoryMiB == spec.MemoryMiB && snap.Network == spec.Network
	case *ownsandv1.SandboxSource_TemplateId:
		// The control plane resolves a template to its snapshot ID before it
		// sends the create command.
		return runtime.VMSpec{}, nil, connect.NewError(connect.CodeInvalidArgument, errors.New("the create command must name the template's snapshot"))
	default:
		return runtime.VMSpec{}, nil, connect.NewError(connect.CodeInvalidArgument, errors.New("the create command has no source"))
	}
	if err := m.checkLimitsLocked(c, spec.GuestOS); err != nil {
		return runtime.VMSpec{}, nil, err
	}
	now := m.cfg.Now()
	rec := &state.SandboxRecord{
		SandboxID:    sid,
		State:        ownsandv1.SandboxState_SANDBOX_STATE_CREATING,
		GuestOS:      spec.GuestOS,
		BaseImage:    spec.Source.BaseImage,
		SnapshotID:   spec.Source.SnapshotID,
		VCPUs:        spec.VCPUs,
		MemoryMiB:    spec.MemoryMiB,
		DiskGiB:      spec.DiskGiB,
		Network:      spec.Network,
		IdleTTL:      c.GetIdleTtl().AsDuration(),
		IdleDeadline: c.GetIdleDeadline().AsTime(),
		MaxDeadline:  c.GetMaxDeadline().AsTime(),
		CreatedAt:    now,
	}
	return spec, rec, nil
}

func contains[T comparable](s []T, v T) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// boot creates and starts the VM, waits for the guest helper and sets the
// guest clock.
func (m *Manager) boot(ctx context.Context, spec runtime.VMSpec) (ownsandv1.RestorePath, error) {
	if err := m.cfg.Runtime.Create(ctx, spec); err != nil {
		return 0, fmt.Errorf("create VM: %w", err)
	}
	path, err := m.cfg.Runtime.Start(ctx, spec.SandboxID)
	if err != nil {
		return 0, fmt.Errorf("start VM: %w", err)
	}
	if _, err := m.Guest(ctx, spec.SandboxID); err != nil {
		return 0, fmt.Errorf("wait for the guest helper: %w", err)
	}
	return path, nil
}

// Guest returns the guest helper connection of a sandbox, dialing a new one
// when there is none. A new connection is checked with a readiness request
// and sets the guest clock.
func (m *Manager) Guest(ctx context.Context, sandboxID string) (*guestconn.Client, error) {
	m.gmu.Lock()
	defer m.gmu.Unlock()
	if gc := m.guests[sandboxID]; gc != nil {
		select {
		case <-gc.Done():
			delete(m.guests, sandboxID)
		default:
			return gc, nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, m.cfg.GuestReadyTimeout)
	defer cancel()
	backoff := 100 * time.Millisecond
	for {
		gc, err := m.dialGuest(ctx, sandboxID)
		if err == nil {
			m.guests[sandboxID] = gc
			return gc, nil
		}
		if connect.CodeOf(err) == connect.CodeUnimplemented {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("guest helper is not ready: %w", err))
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, 2*time.Second)
	}
}

func (m *Manager) dialGuest(ctx context.Context, sandboxID string) (*guestconn.Client, error) {
	rwc, err := m.cfg.Runtime.DialGuest(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	gc := guestconn.New(rwc, m.log.With("sandbox_id", sandboxID))
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := gc.Ready(rctx); err != nil {
		_ = gc.Close()
		return nil, err
	}
	if err := gc.SetClock(m.cfg.Now()); err != nil {
		_ = gc.Close()
		return nil, err
	}
	return gc, nil
}

func (m *Manager) dropGuest(sandboxID string) {
	m.gmu.Lock()
	gc := m.guests[sandboxID]
	delete(m.guests, sandboxID)
	m.gmu.Unlock()
	if gc != nil {
		_ = gc.Close()
	}
}

// delete deletes a sandbox. It is idempotent. A delete for an unknown
// sandbox leaves a delete marker that rejects a later create.
func (m *Manager) delete(sid string, reason ownsandv1.SandboxEndReason) error {
	m.mu.Lock()
	r := m.recs.Sandboxes[sid]
	if r == nil {
		now := m.cfg.Now()
		m.changeLocked(&state.SandboxRecord{
			SandboxID:    sid,
			State:        ownsandv1.SandboxState_SANDBOX_STATE_DELETED,
			EndReason:    reason,
			CreatedAt:    now,
			DeleteMarker: true,
			MarkerUntil:  now.Add(MarkerRetention),
		}, func(*state.SandboxRecord) {})
		m.mu.Unlock()
		m.log.Info("delete before create left a delete marker", "sandbox_id", sid)
		return nil
	}
	if r.Terminal() {
		m.mu.Unlock()
		return nil
	}
	m.changeLocked(r, func(r *state.SandboxRecord) {
		r.State = ownsandv1.SandboxState_SANDBOX_STATE_DELETING
		r.PendingEndReason = reason
	})
	m.mu.Unlock()

	m.dropGuest(sid)
	if err := m.cfg.Runtime.Delete(m.ctx, sid); err != nil {
		m.log.Error("delete VM failed", "sandbox_id", sid, "error", err)
		return connect.NewError(connect.CodeInternal, errors.New("delete VM failed"))
	}
	m.mu.Lock()
	m.changeLocked(r, func(r *state.SandboxRecord) {
		r.State = ownsandv1.SandboxState_SANDBOX_STATE_DELETED
		r.EndReason = reason
	})
	delete(m.expiring, sid)
	delete(m.active, sid)
	delete(m.lastIdleReport, sid)
	m.mu.Unlock()
	return nil
}

func (m *Manager) pause(sid string) error {
	m.mu.Lock()
	r := m.recs.Sandboxes[sid]
	m.mu.Unlock()
	if r == nil || r.DeleteMarker {
		return notFound("sandbox")
	}
	switch r.State {
	case ownsandv1.SandboxState_SANDBOX_STATE_PAUSED:
		return nil
	case ownsandv1.SandboxState_SANDBOX_STATE_RUNNING:
	default:
		return failedPrecondition("only a running sandbox can be paused")
	}
	if err := m.cfg.Runtime.Pause(m.ctx, sid); err != nil {
		m.log.Error("pause VM failed", "sandbox_id", sid, "error", err)
		return connect.NewError(connect.CodeInternal, errors.New("pause VM failed"))
	}
	m.mu.Lock()
	m.changeLocked(r, func(r *state.SandboxRecord) { r.State = ownsandv1.SandboxState_SANDBOX_STATE_PAUSED })
	m.mu.Unlock()
	return nil
}

func (m *Manager) resume(sid string) error {
	m.mu.Lock()
	r := m.recs.Sandboxes[sid]
	m.mu.Unlock()
	if r == nil || r.DeleteMarker {
		return notFound("sandbox")
	}
	switch r.State {
	case ownsandv1.SandboxState_SANDBOX_STATE_RUNNING:
		return nil
	case ownsandv1.SandboxState_SANDBOX_STATE_PAUSED:
	default:
		return failedPrecondition("only a paused sandbox can be resumed")
	}
	if err := m.cfg.Runtime.Resume(m.ctx, sid); err != nil {
		m.log.Error("resume VM failed", "sandbox_id", sid, "error", err)
		return connect.NewError(connect.CodeInternal, errors.New("resume VM failed"))
	}
	// After a resume the guest clock is behind by the paused time.
	if gc, err := m.Guest(m.ctx, sid); err == nil {
		if err := gc.SetClock(m.cfg.Now()); err != nil {
			m.log.Warn("set guest clock after resume failed", "sandbox_id", sid, "error", err)
		}
	} else {
		m.log.Warn("guest helper not reachable after resume", "sandbox_id", sid, "error", err)
	}
	m.mu.Lock()
	m.changeLocked(r, func(r *state.SandboxRecord) {
		r.State = ownsandv1.SandboxState_SANDBOX_STATE_RUNNING
		// A resume is activity: the idle clock restarts.
		r.IdleDeadline = m.cfg.Now().Add(r.IdleTTL)
	})
	m.lastIdleReport[sid] = m.cfg.Now()
	m.mu.Unlock()
	return nil
}

func (m *Manager) snapshot(commandID string, c *ownsandv1.CreateSnapshotCommand) *ownsandv1.AgentCommandResult {
	sid, snapID := c.GetSandboxId(), c.GetSnapshotId()
	m.mu.Lock()
	r := m.recs.Sandboxes[sid]
	existing := m.recs.Snapshots[snapID]
	m.mu.Unlock()
	if existing != nil {
		res := Result(commandID, nil)
		res.Result = &ownsandv1.AgentCommandResult_Snapshot{Snapshot: snapshotReport(existing)}
		return res
	}
	if r == nil || r.DeleteMarker {
		return Result(commandID, notFound("sandbox"))
	}
	if r.State != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING && r.State != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		return Result(commandID, failedPrecondition("only a running or paused sandbox can be snapshotted"))
	}
	info, err := m.cfg.Runtime.Snapshot(m.ctx, sid, snapID)
	if err != nil {
		m.log.Error("snapshot failed", "sandbox_id", sid, "snapshot_id", snapID, "error", err)
		code := connect.CodeOf(err)
		if code == connect.CodeUnknown {
			err = connect.NewError(connect.CodeInternal, errors.New("snapshot failed"))
		}
		return Result(commandID, err)
	}
	rec := &state.SnapshotRecord{
		SnapshotID:      snapID,
		SourceSandboxID: sid,
		State:           ownsandv1.SnapshotState_SNAPSHOT_STATE_READY,
		GuestOS:         r.GuestOS,
		SizeBytes:       info.SizeBytes,
		DiskGiB:         r.DiskGiB,
		HasMemoryState:  info.HasMemoryState,
		VCPUs:           r.VCPUs,
		MemoryMiB:       r.MemoryMiB,
		Network:         r.Network,
		CreatedAt:       m.cfg.Now(),
	}
	m.mu.Lock()
	m.recs.Snapshots[snapID] = rec
	m.saveLocked()
	m.mu.Unlock()
	m.log.Info("snapshot ready", "sandbox_id", sid, "snapshot_id", snapID, "size_bytes", info.SizeBytes, "has_memory_state", info.HasMemoryState)
	rep := snapshotReport(rec)
	m.cfg.Reporter.SnapshotUpdate(&ownsandv1.SnapshotUpdate{Snapshot: rep, ExportState: ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_NOT_EXPORTED})
	res := Result(commandID, nil)
	res.Result = &ownsandv1.AgentCommandResult_Snapshot{Snapshot: rep}
	return res
}

func (m *Manager) deleteSnapshot(snapID string) error {
	if err := m.cfg.Runtime.DeleteSnapshot(m.ctx, snapID); err != nil {
		m.log.Error("delete snapshot failed", "snapshot_id", snapID, "error", err)
		return connect.NewError(connect.CodeInternal, errors.New("delete snapshot failed"))
	}
	m.mu.Lock()
	delete(m.recs.Snapshots, snapID)
	m.saveLocked()
	m.mu.Unlock()
	m.log.Info("snapshot deleted", "snapshot_id", snapID)
	return nil
}
