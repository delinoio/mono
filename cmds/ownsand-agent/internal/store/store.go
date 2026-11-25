// SPDX-License-Identifier: FSL-1.1-ALv2

// Package store owns the agent state directory. The identity file (device
// ID and Ed25519 private key) is kept apart from every other record, so the
// loss of sandbox records does not lose the identity. Every write replaces a
// file atomically (temporary file, fsync, rename).
//
// Layout:
//
//	identity.json             device ID, organization ID, private key (0600)
//	enrollment.lock           lock file of the enrollment sequence (0600)
//	agent.json                control plane origin, state epoch, revision
//	                          counter, removal record, pending update
//	status.json               last status of the running agent (for `status`)
//	sandboxes/<id>.json       sandbox records with TTL deadlines
//	snapshots/<id>/           snapshot record and content
//	execs.json                exec_id records for at-most-once execution
//	vms/<sandbox-id>/         runtime-owned VM files (disks, memory state)
//	images/                   base images
//	update/                   downloaded release artifacts
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/delinoio/mono/internal/fsutil"
)

// removeAll deletes a tree for the Store. It is fsutil.RemoveAll, which also
// deletes a snapshot copy that kept the read-only directory modes of its
// source. It is a variable so that a test can make one removal fail: a
// read-only directory inside the tree no longer fails it. Production code
// never assigns it.
var removeAll = fsutil.RemoveAll

// File and directory names inside the state directory.
const (
	IdentityFile   = "identity.json"
	AgentFile      = "agent.json"
	StatusFile     = "status.json"
	ExecsFile      = "execs.json"
	SandboxesDir   = "sandboxes"
	SnapshotsDir   = "snapshots"
	VMsDir         = "vms"
	ImagesDir      = "images"
	UpdateDir      = "update"
	snapshotRecord = "record.json"
	stagedSnapshot = ".record.pending"
)

// ErrNotFound reports a missing record.
var ErrNotFound = errors.New("store: record not found")

// syncDir flushes a directory's entries to stable storage; see
// syncDirectory. It is a variable so that a test can record the calls or make
// one fail. Production code never assigns it.
var syncDir = syncDirectory

// Store is the agent state directory. It is safe for concurrent use.
type Store struct {
	dir string
	mu  sync.Mutex
	// createdParents lists the parent of every directory that Open created
	// on the way to the state directory (the state directory itself
	// included), deepest first. Their entries are flushed with the first
	// durable identity write.
	createdParents []string
}

// Open creates the state directory and its subdirectories and restricts the
// directory to the service account: mode 0700 on Linux and macOS, and a
// protected ACL for SYSTEM and Administrators on Windows (see
// restrictStateDir).
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("store: state directory is required")
	}
	createdParents := missingDirParents(dir)
	for _, sub := range []string{"", SandboxesDir, SnapshotsDir, VMsDir, ImagesDir, UpdateDir} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return nil, fmt.Errorf("store: create %s: %w", filepath.Join(dir, sub), err)
		}
	}
	// The directory may exist with wider permissions from an operator.
	if err := restrictStateDir(dir); err != nil {
		return nil, fmt.Errorf("store: restrict state directory: %w", err)
	}
	return &Store{dir: dir, createdParents: createdParents}, nil
}

// missingDirParents returns the parent of dir and of each of its ancestors
// that does not exist yet, deepest first. Open is about to create these
// directories.
func missingDirParents(dir string) []string {
	var parents []string
	for p := filepath.Clean(dir); ; {
		if _, err := os.Lstat(p); err == nil || !errors.Is(err, fs.ErrNotExist) {
			break
		}
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		parents = append(parents, parent)
		p = parent
	}
	return parents
}

// Dir returns the state directory.
func (s *Store) Dir() string { return s.dir }

// Path returns a path inside the state directory.
func (s *Store) Path(elem ...string) string {
	return filepath.Join(append([]string{s.dir}, elem...)...)
}

// WriteFileAtomic replaces path with data: it writes a temporary file in the
// same directory, syncs it and renames it into place. On Windows the rename
// retries for a short time while a reader holds the destination open.
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return replaceFile(tmp.Name(), path)
}

func (s *Store) writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, append(data, '\n'), 0o600)
}

func (s *Store) readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("store: decode %s: %w", filepath.Base(path), err)
	}
	return nil
}

// validID rejects record IDs that could escape their directory.
func validID(id string) error {
	if id == "" || strings.ContainsAny(id, `/\.`) {
		return fmt.Errorf("store: invalid record ID %q", id)
	}
	return nil
}

// Identity is the device identity. Secret: the private key never leaves the
// device and is never logged.
type Identity struct {
	DeviceID       string `json:"device_id"`
	OrganizationID string `json:"organization_id"`
	// PrivateKeySeed is the 32-byte Ed25519 seed.
	PrivateKeySeed []byte `json:"private_key_seed"`
}

// LoadIdentity reads the identity file.
func (s *Store) LoadIdentity() (*Identity, error) {
	var id Identity
	if err := s.readJSON(s.Path(IdentityFile), &id); err != nil {
		return nil, err
	}
	return &id, nil
}

// SaveIdentity writes the identity file with mode 0600.
func (s *Store) SaveIdentity(id *Identity) error {
	return s.writeJSON(s.Path(IdentityFile), id)
}

// SaveIdentityDurable writes the identity like SaveIdentity and then, on
// Linux, flushes the directory entries that make the file reachable after a
// power loss: the state directory and the parent of every directory that
// Open created for it. Enrollment uses it for the pending key, which must
// survive before the one-time token is spent: the control plane accepts a
// retry of the token only for the key of the first call. When a flush
// fails, the error is returned and the caller must not go on.
func (s *Store) SaveIdentityDurable(id *Identity) error {
	if err := s.SaveIdentity(id); err != nil {
		return err
	}
	for _, d := range append([]string{s.dir}, s.createdParents...) {
		if err := syncDir(d); err != nil {
			return fmt.Errorf("store: sync directory %s: %w", d, err)
		}
	}
	return nil
}

// DeleteIdentity deletes the device key.
func (s *Store) DeleteIdentity() error {
	err := os.Remove(s.Path(IdentityFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// RemovalRecord records an authenticated removal notice and the progress
// of the clean-up.
type RemovalRecord struct {
	NoticeReceived bool `json:"notice_received"`
	CleanupDone    bool `json:"cleanup_done"`
}

// PendingUpdate records an installed update that the next start confirms.
type PendingUpdate struct {
	TargetVersion   string `json:"target_version"`
	PreviousVersion string `json:"previous_version"`
}

// AgentState is the agent record apart from the identity.
type AgentState struct {
	APIURL string `json:"api_url"`
	// StateEpoch is a UUID v7 created when the agent starts with a lost or
	// reset state directory.
	StateEpoch string `json:"state_epoch"`
	// RevisionCounter is the last sandbox revision counter of the epoch.
	RevisionCounter uint64         `json:"revision_counter"`
	Removal         RemovalRecord  `json:"removal"`
	PendingUpdate   *PendingUpdate `json:"pending_update,omitempty"`
	// Limits and STUN URLs of the last configuration message.
	Limits   *Limits  `json:"limits,omitempty"`
	STUNURLs []string `json:"stun_urls,omitempty"`
}

// Limits mirrors DeviceLimits.
type Limits struct {
	VCPUCount             int32 `json:"vcpu_count"`
	MemoryMiB             int64 `json:"memory_mib"`
	DiskGiB               int64 `json:"disk_gib"`
	VCPUOvercommitPercent int32 `json:"vcpu_overcommit_percent"`
}

// LoadAgent reads agent.json.
func (s *Store) LoadAgent() (*AgentState, error) {
	var st AgentState
	if err := s.readJSON(s.Path(AgentFile), &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// UpdateAgent reads agent.json (empty when missing), applies fn and writes
// the result. Calls are serialized.
func (s *Store) UpdateAgent(fn func(*AgentState) error) (*AgentState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.LoadAgent()
	if errors.Is(err, ErrNotFound) {
		st, err = &AgentState{}, nil
	}
	if err != nil {
		return nil, err
	}
	if err := fn(st); err != nil {
		return nil, err
	}
	if err := s.writeJSON(s.Path(AgentFile), st); err != nil {
		return nil, err
	}
	return st, nil
}

// SaveStatus writes the status file of the running agent.
func (s *Store) SaveStatus(v any) error {
	return s.writeJSON(s.Path(StatusFile), v)
}

// LoadStatus reads the status file into v.
func (s *Store) LoadStatus(v any) error {
	return s.readJSON(s.Path(StatusFile), v)
}

// SaveSandbox writes one sandbox record.
func (s *Store) SaveSandbox(id string, v any) error {
	if err := validID(id); err != nil {
		return err
	}
	return s.writeJSON(s.Path(SandboxesDir, id+".json"), v)
}

// DeleteSandbox deletes one sandbox record.
func (s *Store) DeleteSandbox(id string) error {
	if err := validID(id); err != nil {
		return err
	}
	err := os.Remove(s.Path(SandboxesDir, id+".json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// LoadSandboxes decodes every sandbox record with decode.
func (s *Store) LoadSandboxes(decode func(data []byte) error) error {
	entries, err := os.ReadDir(s.Path(SandboxesDir))
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		data, err := os.ReadFile(s.Path(SandboxesDir, e.Name()))
		if err != nil {
			return err
		}
		if err := decode(data); err != nil {
			return fmt.Errorf("store: decode sandbox record %s: %w", e.Name(), err)
		}
	}
	return nil
}

// SnapshotDir returns the directory of one snapshot.
func (s *Store) SnapshotDir(id string) (string, error) {
	if err := validID(id); err != nil {
		return "", err
	}
	return s.Path(SnapshotsDir, id), nil
}

// SaveSnapshot writes the record of one snapshot.
func (s *Store) SaveSnapshot(id string, v any) error {
	return s.writeSnapshotRecord(id, snapshotRecord, v)
}

// StageSnapshot saves a capture record under a name that LoadSnapshots ignores.
func (s *Store) StageSnapshot(id string, v any) error {
	return s.writeSnapshotRecord(id, stagedSnapshot, v)
}

func (s *Store) writeSnapshotRecord(id, name string, v any) error {
	dir, err := s.SnapshotDir(id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return s.writeJSON(filepath.Join(dir, name), v)
}

// PublishSnapshot installs the staged capture record under the loader's name.
// The caller serializes publication with the decision to discard the capture.
func (s *Store) PublishSnapshot(id string) error {
	dir, err := s.SnapshotDir(id)
	if err != nil {
		return err
	}
	return replaceFile(filepath.Join(dir, stagedSnapshot), filepath.Join(dir, snapshotRecord))
}

// DeleteSnapshot deletes a snapshot with its content.
func (s *Store) DeleteSnapshot(id string) error {
	dir, err := s.SnapshotDir(id)
	if err != nil {
		return err
	}
	return removeAll(dir)
}

// transferArchivePattern matches the temporary archives that snapxfer creates
// in snapshots/ with os.CreateTemp: ".export-<id>-<digits>.tar" and
// ".import-<id>-<digits>.tar". Keep it in step with the prefixes in snapxfer.
var transferArchivePattern = regexp.MustCompile(`^\.(export|import)-.+-[0-9]+\.tar$`)

// LoadSnapshots decodes every snapshot record with decode. A snapshot
// directory without a record is removed: it is a partial import or
// snapshot. A regular file that matches a snapshot transfer archive name is
// removed too: a killed process leaves it, because snapxfer removes it only
// with a deferred call. Other files stay.
func (s *Store) LoadSnapshots(decode func(data []byte) error) error {
	entries, err := os.ReadDir(s.Path(SnapshotsDir))
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			if e.Type().IsRegular() && transferArchivePattern.MatchString(e.Name()) {
				if err := os.Remove(s.Path(SnapshotsDir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return fmt.Errorf("store: remove abandoned transfer archive %s: %w", e.Name(), err)
				}
			}
			continue
		}
		data, err := os.ReadFile(s.Path(SnapshotsDir, e.Name(), snapshotRecord))
		if errors.Is(err, fs.ErrNotExist) {
			if err := removeAll(s.Path(SnapshotsDir, e.Name())); err != nil {
				return fmt.Errorf("store: remove incomplete snapshot %s: %w", e.Name(), err)
			}
			continue
		}
		if err != nil {
			return err
		}
		if err := decode(data); err != nil {
			return fmt.Errorf("store: decode snapshot record %s: %w", e.Name(), err)
		}
	}
	return nil
}

// SaveExecs writes the exec records.
func (s *Store) SaveExecs(v any) error {
	return s.writeJSON(s.Path(ExecsFile), v)
}

// LoadExecs reads the exec records into v.
func (s *Store) LoadExecs(v any) error {
	return s.readJSON(s.Path(ExecsFile), v)
}

// WipeLocalData deletes every sandbox record, snapshot, VM file, base image
// and update download, and the exec records. It keeps the identity and
// agent.json.
func (s *Store) WipeLocalData() error {
	for _, sub := range []string{SandboxesDir, SnapshotsDir, VMsDir, ImagesDir, UpdateDir} {
		if err := removeAll(s.Path(sub)); err != nil {
			return err
		}
		if err := os.MkdirAll(s.Path(sub), 0o700); err != nil {
			return err
		}
	}
	for _, f := range []string{ExecsFile, StatusFile} {
		if err := os.Remove(s.Path(f)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Reset deletes local state, including the identity, under the enrollment
// lock. See ResetDir for the cleanup and error rules.
func (s *Store) Reset() error {
	return ResetDir(s.dir)
}
