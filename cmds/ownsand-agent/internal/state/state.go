// Package state owns the agent state directory: the identity file, the
// sandbox and snapshot records with their TTL deadlines, the state epoch, the
// removal record and the status file that `ownsand-agent status` reads.
//
// The identity file is kept apart from the other records, so the loss of the
// records does not lose the identity. Every file is written atomically
// (temporary file, fsync, rename) with mode 0600, and the directory has mode
// 0700, so only the agent's service account can read it.
package state

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

const (
	identityFile = "identity.json"
	recordsFile  = "records.json"
	removalFile  = "removal.json"
	statusFile   = "status.json"
	// DefaultOrigin is the production control plane origin.
	DefaultOrigin = "https://ownsand.api.delino.io"
)

// ErrNotEnrolled reports a state directory without a complete identity.
var ErrNotEnrolled = errors.New("the device is not enrolled")

// DefaultDir returns the platform state directory.
func DefaultDir() string {
	switch runtime.GOOS {
	case "darwin":
		return "/Library/Application Support/ownsand-agent"
	case "windows":
		pd := os.Getenv("ProgramData")
		if pd == "" {
			pd = `C:\ProgramData`
		}
		return filepath.Join(pd, "ownsand-agent")
	default:
		return "/var/lib/ownsand-agent"
	}
}

// Dir is an agent state directory.
type Dir struct {
	Path string
	mu   sync.Mutex
}

// Open creates the directory when it does not exist.
func Open(path string) (*Dir, error) {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	return &Dir{Path: path}, nil
}

// Sub returns a subdirectory path, created with mode 0700.
func (d *Dir) Sub(name string) (string, error) {
	p := filepath.Join(d.Path, name)
	return p, os.MkdirAll(p, 0o700)
}

// writeJSON writes v atomically.
func (d *Dir) writeJSON(name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(filepath.Join(d.Path, name), data, 0o600)
}

func (d *Dir) readJSON(name string, v any) error {
	data, err := os.ReadFile(filepath.Join(d.Path, name))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// WriteFileAtomic writes data to path through a temporary file and a rename.
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Identity is the device identity. The private key never leaves the device.
type Identity struct {
	// DeviceID is empty while an enrollment is pending: the key pair exists,
	// and Enroll has not returned yet. A retry reuses the same key, so a
	// lost Enroll response does not need a new token.
	DeviceID       string             `json:"device_id,omitempty"`
	OrganizationID string             `json:"organization_id,omitempty"`
	Origin         string             `json:"origin"`
	PrivateKey     ed25519.PrivateKey `json:"private_key"`
	EnrolledAt     time.Time          `json:"enrolled_at,omitzero"`
}

// Enrolled reports whether the identity is complete.
func (i *Identity) Enrolled() bool {
	return i != nil && i.DeviceID != "" && len(i.PrivateKey) == ed25519.PrivateKeySize
}

// PublicKey returns the raw 32-byte public key.
func (i *Identity) PublicKey() ed25519.PublicKey {
	return i.PrivateKey.Public().(ed25519.PublicKey)
}

// LoadIdentity reads the identity file. It returns fs.ErrNotExist when the
// file does not exist.
func (d *Dir) LoadIdentity() (*Identity, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var id Identity
	if err := d.readJSON(identityFile, &id); err != nil {
		return nil, err
	}
	return &id, nil
}

// SaveIdentity writes the identity file.
func (d *Dir) SaveIdentity(id *Identity) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.writeJSON(identityFile, id)
}

// DeleteIdentity deletes the device key. Removal and reset call it last.
func (d *Dir) DeleteIdentity() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	err := os.Remove(filepath.Join(d.Path, identityFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// SandboxRecord is the persisted record of one sandbox.
type SandboxRecord struct {
	SandboxID  string                     `json:"sandbox_id"`
	State      ownsandv1.SandboxState     `json:"state"`
	EndReason  ownsandv1.SandboxEndReason `json:"end_reason,omitempty"`
	GuestOS    ownsandv1.GuestOs          `json:"guest_os"`
	BaseImage  ownsandv1.BaseImage        `json:"base_image,omitempty"`
	SnapshotID string                     `json:"snapshot_id,omitempty"`
	VCPUs      int64                      `json:"vcpu_count"`
	MemoryMiB  int64                      `json:"memory_mib"`
	DiskGiB    int64                      `json:"disk_gib"`
	Network    ownsandv1.NetworkMode      `json:"network_mode"`
	IdleTTL    time.Duration              `json:"idle_ttl"`
	// IdleDeadline and MaxDeadline are wall-clock deadlines, so they survive
	// agent restarts. The idle clock does not run while the sandbox is paused.
	IdleDeadline time.Time             `json:"idle_deadline"`
	MaxDeadline  time.Time             `json:"max_deadline"`
	Counter      uint64                `json:"revision_counter"`
	RestorePath  ownsandv1.RestorePath `json:"restore_path,omitempty"`
	CreatedAt    time.Time             `json:"created_at"`
	// DeleteMarker is set for a delete that arrived before its create; the
	// agent rejects that create. MarkerUntil bounds how long it is kept.
	DeleteMarker bool      `json:"delete_marker,omitempty"`
	MarkerUntil  time.Time `json:"marker_until,omitzero"`
	// PendingEndReason is the end reason of a delete that is in progress
	// (state DELETING), so a restart completes it with the same reason.
	PendingEndReason ownsandv1.SandboxEndReason `json:"pending_end_reason,omitempty"`
	// AckedCounter is the highest revision counter that the control plane
	// acknowledged.
	AckedCounter uint64 `json:"acked_counter,omitempty"`
}

// Terminal reports whether the sandbox ended.
func (r *SandboxRecord) Terminal() bool {
	return r.State == ownsandv1.SandboxState_SANDBOX_STATE_DELETED || r.State == ownsandv1.SandboxState_SANDBOX_STATE_FAILED
}

// SnapshotRecord is the persisted record of one local snapshot.
type SnapshotRecord struct {
	SnapshotID      string                  `json:"snapshot_id"`
	SourceSandboxID string                  `json:"source_sandbox_id"`
	State           ownsandv1.SnapshotState `json:"state"`
	GuestOS         ownsandv1.GuestOs       `json:"guest_os"`
	SizeBytes       int64                   `json:"size_bytes"`
	DiskGiB         int64                   `json:"disk_gib"`
	HasMemoryState  bool                    `json:"has_memory_state"`
	VCPUs           int64                   `json:"vcpu_count"`
	MemoryMiB       int64                   `json:"memory_mib"`
	Network         ownsandv1.NetworkMode   `json:"network_mode"`
	CreatedAt       time.Time               `json:"created_at"`
}

// Records is the content of the records file.
type Records struct {
	// Epoch is the state epoch: a new UUID v7 whenever the agent starts with
	// a lost or reset records file.
	Epoch     string                     `json:"epoch"`
	Sandboxes map[string]*SandboxRecord  `json:"sandboxes"`
	Snapshots map[string]*SnapshotRecord `json:"snapshots"`
}

// LoadRecords reads the records file. A missing or unreadable file starts a
// new epoch, and fresh reports whether it did.
func (d *Dir) LoadRecords() (recs *Records, fresh bool, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	recs = &Records{}
	rerr := d.readJSON(recordsFile, recs)
	if rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
		// A corrupt records file counts as lost state: keep a copy for the
		// operator and start a new epoch.
		_ = os.Rename(filepath.Join(d.Path, recordsFile), filepath.Join(d.Path, recordsFile+".corrupt"))
	}
	if rerr != nil || recs.Epoch == "" {
		recs = &Records{Epoch: datapath.NewID()}
		fresh = true
	}
	if recs.Sandboxes == nil {
		recs.Sandboxes = map[string]*SandboxRecord{}
	}
	if recs.Snapshots == nil {
		recs.Snapshots = map[string]*SnapshotRecord{}
	}
	if fresh {
		if err := d.writeJSON(recordsFile, recs); err != nil {
			return nil, false, err
		}
	}
	return recs, fresh, nil
}

// SaveRecords writes the records file.
func (d *Dir) SaveRecords(recs *Records) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.writeJSON(recordsFile, recs)
}

// Removal records an authenticated removal notice and the clean-up.
type Removal struct {
	NoticeReceivedAt time.Time `json:"notice_received_at"`
	CleanedUp        bool      `json:"cleaned_up"`
}

// LoadRemoval returns the removal record, or nil when there is none.
func (d *Dir) LoadRemoval() (*Removal, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var r Removal
	if err := d.readJSON(removalFile, &r); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return &r, nil
}

// SaveRemoval writes the removal record.
func (d *Dir) SaveRemoval(r *Removal) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.writeJSON(removalFile, r)
}

// ConnectionState is the agent channel state shown by `status`.
type ConnectionState string

const (
	ConnectionDisconnected ConnectionState = "disconnected"
	ConnectionConnecting   ConnectionState = "connecting"
	ConnectionConnected    ConnectionState = "connected"
	ConnectionRemoved      ConnectionState = "removed"
)

// Status is what `ownsand-agent run` last wrote for `status`.
type Status struct {
	UpdatedAt        time.Time                  `json:"updated_at"`
	Connection       ConnectionState            `json:"connection"`
	LastConnectedAt  time.Time                  `json:"last_connected_at,omitzero"`
	LastError        string                     `json:"last_error,omitempty"`
	AgentVersion     string                     `json:"agent_version"`
	UpdateState      ownsandv1.AgentUpdateState `json:"update_state"`
	TargetVersion    string                     `json:"target_version,omitempty"`
	Capabilities     *CapabilitySummary         `json:"capabilities,omitempty"`
	Limits           *ownsandv1.DeviceLimits    `json:"limits,omitempty"`
	SandboxStates    map[string]int             `json:"sandbox_states,omitempty"`
	HypervisorChecks map[string]string          `json:"hypervisor_checks,omitempty"`
	BuiltBaseImages  []ownsandv1.BaseImage      `json:"built_base_images,omitempty"`
	GuestOSes        []ownsandv1.GuestOs        `json:"guest_oses,omitempty"`
}

// CapabilitySummary is the host part of the capability report.
type CapabilitySummary struct {
	HostOS         string `json:"host_os"`
	Arch           string `json:"arch"`
	LogicalCPUs    int64  `json:"logical_cpu_count"`
	MemoryMiB      int64  `json:"memory_mib"`
	FreeDiskBytes  int64  `json:"free_disk_bytes"`
	MacOSSupported bool   `json:"macos_guests_supported"`
}

// SaveStatus writes the status file.
func (d *Dir) SaveStatus(s *Status) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.writeJSON(statusFile, s)
}

// LoadStatus reads the status file, or returns nil when there is none.
func (d *Dir) LoadStatus() (*Status, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var s Status
	if err := d.readJSON(statusFile, &s); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return &s, nil
}

// Reset deletes everything in the state directory, including the identity,
// and keeps the directory itself.
func (d *Dir) Reset() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	entries, err := os.ReadDir(d.Path)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(d.Path, e.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
