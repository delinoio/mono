// SPDX-License-Identifier: Apache-2.0

package sandboxes

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/delinoio/mono/packages/ownsand-sdk-go/validate"
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
	m.mu.Lock()
	if snap, ok := m.snapshots[cmd.GetSnapshotId()]; ok {
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
	m.mu.Unlock()
	if state != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING && state != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		return nil, cmdErr(ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, "sandbox is %s", state)
	}
	dir, err := m.st.SnapshotDir(cmd.GetSnapshotId())
	if err != nil {
		return nil, cmdErr(ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "%v", err)
	}
	if err := removeDirIfExists(dir); err != nil {
		return nil, cmdErr(ownsandv1.ErrorCode_ERROR_CODE_INTERNAL, "snapshot directory could not be prepared")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, cmdErr(ownsandv1.ErrorCode_ERROR_CODE_INTERNAL, "snapshot directory could not be created")
	}
	m.logger.Info("snapshot creating", "snapshot_id", cmd.GetSnapshotId(), "sandbox_id", cmd.GetSandboxId())
	info, err := m.rt.Snapshot(ctx, vm, filepath.Join(dir, contentDir))
	if err != nil {
		os.RemoveAll(dir)
		m.logger.Error("snapshot failed", "snapshot_id", cmd.GetSnapshotId(), "error", err)
		return nil, cmdErr(ownsandv1.ErrorCode_ERROR_CODE_INTERNAL, "snapshot could not be taken")
	}
	snap := &snapshot{dir: filepath.Join(dir, contentDir), rec: SnapshotRecord{
		SnapshotID:      cmd.GetSnapshotId(),
		SourceSandboxID: cmd.GetSandboxId(),
		State:           ownsandv1.SnapshotState_SNAPSHOT_STATE_READY,
		GuestOS:         rec.GuestOS,
		VCPUCount:       rec.VCPUCount,
		MemoryMiB:       rec.MemoryMiB,
		DiskGiB:         rec.DiskGiB,
		NetworkMode:     rec.NetworkMode,
		HasMemoryState:  info.HasMemoryState,
		SizeBytes:       info.SizeBytes,
	}}
	if err := m.st.SaveSnapshot(snap.rec.SnapshotID, snap.rec); err != nil {
		os.RemoveAll(dir)
		return nil, cmdErr(ownsandv1.ErrorCode_ERROR_CODE_INTERNAL, "snapshot record could not be saved")
	}
	m.mu.Lock()
	m.snapshots[snap.rec.SnapshotID] = snap
	m.mu.Unlock()
	m.logger.Info("snapshot ready", "snapshot_id", snap.rec.SnapshotID, "size_bytes", snap.rec.SizeBytes, "has_memory_state", snap.rec.HasMemoryState)
	return snap.entry(), nil
}

// SnapshotContent returns the content directory and entry of a ready local
// snapshot.
func (m *Manager) SnapshotContent(id string) (string, *ownsandv1.SnapshotStateEntry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	snap, ok := m.snapshots[id]
	if !ok || snap.rec.State != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY {
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

// BeginTransfer marks a running export or import; the returned function
// ends it.
func (m *Manager) BeginTransfer() func() {
	m.mu.Lock()
	m.transfers++
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		m.transfers--
		m.mu.Unlock()
	}
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
