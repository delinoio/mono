// SPDX-License-Identifier: FSL-1.1-ALv2

package sandboxes

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/delinoio/mono/go/ownsand/validate"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// contentDir is the directory inside a snapshot directory that holds what
// the runtime wrote.
const contentDir = "content"

// SnapshotRecord is the persisted record of a local snapshot.
type SnapshotRecord struct {
	SnapshotID         string                  `json:"snapshot_id"`
	SourceSandboxID    string                  `json:"source_sandbox_id,omitempty"`
	State              ownsandv1.SnapshotState `json:"state"`
	GuestOS            ownsandv1.GuestOs       `json:"guest_os"`
	VCPUCount          int32                   `json:"vcpu_count"`
	MemoryMiB          int64                   `json:"memory_mib"`
	DiskGiB            int64                   `json:"disk_gib"`
	NetworkMode        ownsandv1.NetworkMode   `json:"network_mode"`
	HasMemoryState     bool                    `json:"has_memory_state"`
	SizeBytes          int64                   `json:"size_bytes"`
	MacOSHardwareModel []byte                  `json:"macos_hardware_model,omitempty"`
}

type snapshot struct {
	rec SnapshotRecord
	// dir is the content directory.
	dir string
	// users counts the creates and exports that read the content now; a
	// delete waits until it is zero.
	users int
	// deleting is set when a delete arrived while users was not zero. The
	// snapshot takes no new users, and the last user deletes it.
	deleting bool
}

// pendingSnapshot marks a snapshot whose content the runtime is capturing.
// It is kept in Manager.pending, not in Manager.snapshots, so that the
// snapshot is not usable and not reported before it is complete.
type pendingSnapshot struct {
	// deleted is set, under Manager.mu, when a delete arrived during the
	// capture. The create then publishes nothing and removes its content.
	deleted bool
}

// usable reports whether a new create or export may read the snapshot.
func (s *snapshot) usable() bool {
	return s.rec.State == ownsandv1.SnapshotState_SNAPSHOT_STATE_READY && !s.deleting
}

func (s *snapshot) resources() *ownsandv1.Resources {
	return &ownsandv1.Resources{VcpuCount: s.rec.VCPUCount, MemoryMib: s.rec.MemoryMiB, DiskGib: s.rec.DiskGiB}
}

func (s *snapshot) entry() *ownsandv1.SnapshotStateEntry {
	return &ownsandv1.SnapshotStateEntry{
		SnapshotId:         s.rec.SnapshotID,
		SourceSandboxId:    s.rec.SourceSandboxID,
		State:              s.rec.State,
		GuestOs:            s.rec.GuestOS,
		Resources:          s.resources(),
		NetworkMode:        s.rec.NetworkMode,
		HasMemoryState:     s.rec.HasMemoryState,
		SizeBytes:          s.rec.SizeBytes,
		MacosHardwareModel: s.rec.MacOSHardwareModel,
	}
}

func (m *Manager) loadSnapshots() error {
	return m.st.LoadSnapshots(func(data []byte) error {
		var rec SnapshotRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return err
		}
		dir, err := m.st.SnapshotDir(rec.SnapshotID)
		if err != nil {
			return err
		}
		if rec.State != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY {
			// A snapshot that was being taken when the agent stopped is
			// incomplete.
			return m.st.DeleteSnapshot(rec.SnapshotID)
		}
		m.snapshots[rec.SnapshotID] = &snapshot{rec: rec, dir: filepath.Join(dir, contentDir)}
		return nil
	})
}

// CreateSnapshot takes a snapshot of a running or paused sandbox.
func (m *Manager) CreateSnapshot(ctx context.Context, cmd *ownsandv1.CreateSnapshotCommand) (*ownsandv1.SnapshotStateEntry, error) {
	if err := validate.UUIDv7("snapshot_id", cmd.GetSnapshotId()); err != nil {
		return nil, cmdErr(ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
	}
	id := cmd.GetSnapshotId()
	m.mu.Lock()
	if snap, ok := m.snapshots[id]; ok {
		m.mu.Unlock()
		return snap.entry(), nil
	}
	sb, ok := m.sandboxes[cmd.GetSandboxId()]
	m.mu.Unlock()
	if !ok {
		return nil, cmdErr(ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND, "sandbox is unknown on this device")
	}
	sb.cmdMu.Lock()
	defer sb.cmdMu.Unlock()
	m.mu.Lock()
	state, vm, rec := sb.rec.State, sb.vm, sb.rec
	// A duplicate create that waited for the sandbox lock finds the
	// snapshot that the first create published.
	if snap, ok := m.snapshots[id]; ok {
		m.mu.Unlock()
		return snap.entry(), nil
	}
	m.mu.Unlock()
	if state != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING && state != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		return nil, cmdErr(ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, "sandbox is %s", state)
	}
	if vm == nil {
		// A record kept live for a pending restart cleanup has no VM.
		return nil, cmdErr(ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, "sandbox VM is not available")
	}
	dir, err := m.st.SnapshotDir(id)
	if err != nil {
		return nil, cmdErr(ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
	}
	// Register the capture before it starts, so that a delete that arrives
	// meanwhile can record its intent.
	pending := &pendingSnapshot{}
	m.mu.Lock()
	if _, busy := m.pending[id]; busy {
		m.mu.Unlock()
		return nil, cmdErr(ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, "snapshot is being created")
	}
	m.pending[id] = pending
	m.mu.Unlock()
	finish := func() {
		m.mu.Lock()
		if m.pending[id] == pending {
			delete(m.pending, id)
		}
		m.mu.Unlock()
	}
	// published is true once the snapshot is in m.snapshots; until then
	// the pending entry stays, so a delete cannot miss the create.
	published := false
	defer func() {
		if !published {
			finish()
		}
	}()
	if err := removeDirIfExists(dir); err != nil {
		return nil, cmdErr(ownsandv1.ErrorCode_ERROR_CODE_INTERNAL, "snapshot directory could not be prepared")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, cmdErr(ownsandv1.ErrorCode_ERROR_CODE_INTERNAL, "snapshot directory could not be created")
	}
	m.logger.Info("snapshot creating", "snapshot_id", id, "sandbox_id", cmd.GetSandboxId())
	info, err := m.rt.Snapshot(ctx, vm, filepath.Join(dir, contentDir))
	if err != nil {
		os.RemoveAll(dir)
		m.logger.Error("snapshot failed", "snapshot_id", id, "error", err)
		return nil, cmdErr(ownsandv1.ErrorCode_ERROR_CODE_INTERNAL, "snapshot could not be taken")
	}
	if m.snapshotDeletedDuringCreate(pending) {
		return nil, m.abortSnapshotCreate(id, dir)
	}
	snap := &snapshot{dir: filepath.Join(dir, contentDir), rec: SnapshotRecord{
		SnapshotID:         cmd.GetSnapshotId(),
		SourceSandboxID:    cmd.GetSandboxId(),
		State:              ownsandv1.SnapshotState_SNAPSHOT_STATE_READY,
		GuestOS:            rec.GuestOS,
		VCPUCount:          rec.VCPUCount,
		MemoryMiB:          rec.MemoryMiB,
		DiskGiB:            rec.DiskGiB,
		NetworkMode:        rec.NetworkMode,
		HasMemoryState:     info.HasMemoryState,
		SizeBytes:          info.SizeBytes,
		MacOSHardwareModel: info.MacOSHardwareModel,
	}}
	if err := m.st.SaveSnapshot(snap.rec.SnapshotID, snap.rec); err != nil {
		os.RemoveAll(dir)
		return nil, cmdErr(ownsandv1.ErrorCode_ERROR_CODE_INTERNAL, "snapshot record could not be saved")
	}
	// A delete can also arrive while the record is saved. Check again under
	// the lock that publishes the snapshot, so that exactly one of the two
	// wins: either the delete is recorded here, or the delete finds the
	// published snapshot and deletes it.
	m.mu.Lock()
	if pending.deleted {
		delete(m.pending, id)
		m.mu.Unlock()
		published = true // The pending entry is already removed.
		_ = m.st.DeleteSnapshot(id)
		return nil, m.abortedSnapshotError(id)
	}
	m.snapshots[snap.rec.SnapshotID] = snap
	delete(m.pending, id)
	m.mu.Unlock()
	published = true
	m.logger.Info("snapshot ready", "snapshot_id", snap.rec.SnapshotID, "size_bytes", snap.rec.SizeBytes, "has_memory_state", snap.rec.HasMemoryState)
	return snap.entry(), nil
}

// snapshotDeletedDuringCreate reports whether a delete arrived while the
// capture of pending ran.
func (m *Manager) snapshotDeletedDuringCreate(pending *pendingSnapshot) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return pending.deleted
}

// abortSnapshotCreate removes the content of a capture that a delete
// overtook and returns the error of the create. The pending entry is
// removed by the caller's deferred cleanup.
func (m *Manager) abortSnapshotCreate(id, dir string) error {
	if err := removeDirIfExists(dir); err != nil {
		m.logger.Error("aborted snapshot content could not be removed", "snapshot_id", id, "error", err)
	}
	return m.abortedSnapshotError(id)
}

func (m *Manager) abortedSnapshotError(id string) error {
	m.logger.Info("snapshot create aborted by a delete", "snapshot_id", id)
	return cmdErr(ownsandv1.ErrorCode_ERROR_CODE_ABORTED, "snapshot was deleted during its create")
}

// AcquireSnapshot returns the content directory of a ready local snapshot
// and keeps the snapshot from being deleted until release is called.
func (m *Manager) AcquireSnapshot(id string) (dir string, release func(), ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	snap, ok := m.snapshots[id]
	if !ok || !snap.usable() {
		return "", nil, false
	}
	snap.users++
	return snap.dir, m.snapshotReleaser(snap), true
}

// snapshotReleaser returns a function that ends one use of snap. The
// caller holds m.mu when it increments snap.users. The last use of a
// snapshot whose delete was deferred deletes it.
func (m *Manager) snapshotReleaser(snap *snapshot) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			snap.users--
			id := snap.rec.SnapshotID
			remove := snap.deleting && snap.users == 0 && m.snapshots[id] == snap
			if remove {
				delete(m.snapshots, id)
			}
			m.mu.Unlock()
			if remove {
				// The command result was sent when the delete was deferred,
				// so a failure here is only logged; the copy stays on disk
				// until a restart reloads it and the control plane sends the
				// delete again after the next state report.
				_ = m.removeSnapshot(id)
			}
		})
	}
}

// removeSnapshot deletes the stored record and content of a snapshot that
// is no longer in m.snapshots.
func (m *Manager) removeSnapshot(id string) error {
	if err := m.st.DeleteSnapshot(id); err != nil {
		m.logger.Error("snapshot delete failed", "snapshot_id", id, "error", err)
		return cmdErr(ownsandv1.ErrorCode_ERROR_CODE_INTERNAL, "snapshot could not be deleted")
	}
	m.logger.Info("snapshot deleted", "snapshot_id", id)
	return nil
}

// DeleteSnapshot deletes the local copy of a snapshot whose record the
// control plane deleted. It succeeds when the device holds no copy. While
// a create or an export reads the copy, the snapshot takes no new users and
// the last of the current users deletes it; the command succeeds at once,
// because the control plane sends it only once while the agent channel
// stays connected. A delete that arrives while the snapshot is being
// created also succeeds at once; the create then aborts with ABORTED and
// publishes nothing.
func (m *Manager) DeleteSnapshot(cmd *ownsandv1.DeleteSnapshotCommand) error {
	id := cmd.GetSnapshotId()
	if err := validate.UUIDv7("snapshot_id", id); err != nil {
		return cmdErr(ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
	}
	m.mu.Lock()
	snap, ok := m.snapshots[id]
	if !ok {
		if pending, capturing := m.pending[id]; capturing {
			// The snapshot is being captured: record the delete, and the
			// create discards the content instead of publishing it.
			pending.deleted = true
			m.mu.Unlock()
			m.logger.Info("snapshot delete recorded during its create", "snapshot_id", id)
			return nil
		}
		m.mu.Unlock()
		return nil
	}
	if snap.users > 0 {
		snap.deleting = true
		users := snap.users
		m.mu.Unlock()
		m.logger.Info("snapshot delete deferred until its readers end", "snapshot_id", id, "users", users)
		return nil
	}
	delete(m.snapshots, id)
	m.mu.Unlock()
	return m.removeSnapshot(id)
}

// SnapshotContent returns the content directory and entry of a ready local
// snapshot.
func (m *Manager) SnapshotContent(id string) (string, *ownsandv1.SnapshotStateEntry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	snap, ok := m.snapshots[id]
	if !ok || !snap.usable() {
		return "", nil, false
	}
	return snap.dir, snap.entry(), true
}

// HasSnapshot reports whether a snapshot record exists.
func (m *Manager) HasSnapshot(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.snapshots[id]
	return ok
}

// importLock is the lock of one snapshot ID. held is a one-slot semaphore, so
// that a waiting import can give up when its context ends. refs counts the
// imports that hold or wait for the lock and is guarded by Manager.mu; the
// entry leaves Manager.importLocks when it reaches zero.
type importLock struct {
	held chan struct{}
	refs int
}

// LockSnapshotImport serializes the imports of one snapshot ID on the
// device: it blocks until no other import of that ID holds the lock, and the
// returned function releases it. Imports of different IDs do not block each
// other. The caller holds the lock from its check for a ready copy until the
// copy is published with AddImportedSnapshot or its partial content is
// removed, so that a second import of the same ID sees the published copy
// (and reuses it) or the cleaned destination, and never deletes or rewrites
// content that another import publishes. The wait ends with an
// ABORTED error when ctx ends first, and a ctx that already ended returns
// ABORTED even when the lock is free.
func (m *Manager) LockSnapshotImport(ctx context.Context, id string) (func(), error) {
	m.mu.Lock()
	l := m.importLocks[id]
	if l == nil {
		l = &importLock{held: make(chan struct{}, 1)}
		m.importLocks[id] = l
	}
	l.refs++
	m.mu.Unlock()
	drop := func() {
		m.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(m.importLocks, id)
		}
		m.mu.Unlock()
	}
	select {
	case l.held <- struct{}{}:
		// select picks a ready case at random, so a free lock and a context
		// that already ended can both be ready and the lock can win. An import
		// whose context ended must not proceed: give the token back and abort.
		if ctx.Err() != nil {
			<-l.held
			drop()
			return nil, cmdErr(ownsandv1.ErrorCode_ERROR_CODE_ABORTED, "snapshot import was canceled before it acquired the snapshot lock")
		}
	case <-ctx.Done():
		drop()
		return nil, cmdErr(ownsandv1.ErrorCode_ERROR_CODE_ABORTED, "snapshot import was canceled while it waited for another import of the same snapshot")
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			<-l.held
			drop()
		})
	}, nil
}

// ImportDir returns the snapshot directory that an import fills; its
// content goes into the "content" subdirectory.
func (m *Manager) ImportDir(id string) (string, error) {
	return m.st.SnapshotDir(id)
}

// AddImportedSnapshot records an imported snapshot as ready after its
// content was verified and extracted into ImportDir(id)/content.
func (m *Manager) AddImportedSnapshot(entry *ownsandv1.SnapshotStateEntry) (*ownsandv1.SnapshotStateEntry, error) {
	dir, err := m.st.SnapshotDir(entry.GetSnapshotId())
	if err != nil {
		return nil, err
	}
	snap := &snapshot{dir: filepath.Join(dir, contentDir), rec: SnapshotRecord{
		SnapshotID:         entry.GetSnapshotId(),
		SourceSandboxID:    entry.GetSourceSandboxId(),
		State:              ownsandv1.SnapshotState_SNAPSHOT_STATE_READY,
		GuestOS:            entry.GetGuestOs(),
		VCPUCount:          entry.GetResources().GetVcpuCount(),
		MemoryMiB:          entry.GetResources().GetMemoryMib(),
		DiskGiB:            entry.GetResources().GetDiskGib(),
		NetworkMode:        entry.GetNetworkMode(),
		HasMemoryState:     entry.GetHasMemoryState(),
		SizeBytes:          entry.GetSizeBytes(),
		MacOSHardwareModel: entry.GetMacosHardwareModel(),
	}}
	if err := m.st.SaveSnapshot(snap.rec.SnapshotID, snap.rec); err != nil {
		return nil, fmt.Errorf("sandboxes: save imported snapshot: %w", err)
	}
	m.mu.Lock()
	m.snapshots[snap.rec.SnapshotID] = snap
	m.mu.Unlock()
	return snap.entry(), nil
}

// MacOSHardwareModels returns the distinct hardware models of the usable
// local macOS snapshots.
func (m *Manager) MacOSHardwareModels() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	var models [][]byte
	seen := map[string]bool{}
	for _, snap := range m.snapshots {
		model := snap.rec.MacOSHardwareModel
		if !snap.usable() || snap.rec.GuestOS != ownsandv1.GuestOs_GUEST_OS_MACOS || len(model) == 0 || seen[string(model)] {
			continue
		}
		seen[string(model)] = true
		models = append(models, model)
	}
	return models
}
