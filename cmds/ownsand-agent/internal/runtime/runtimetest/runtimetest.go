// Package runtimetest provides a fake runtime for tests. It runs an
// in-process guest helper (the real guest.Server) and connects to it over
// net.Pipe, so tests exercise the guest protocol without a VM.
//
// Only _test.go files import this package. The agent's production runtime
// selection (runtime.New) cannot return it, so it is not selectable from
// production configuration.
package runtimetest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/runtime"
	"github.com/delinoio/mono/cmds/ownsand-guest/guest"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// VMState is the state of a fake VM.
type VMState int

const (
	VMCreated VMState = iota + 1
	VMRunning
	VMPaused
	VMStopped
)

type vm struct {
	spec  runtime.VMSpec
	state VMState
	guest *guest.Server
	ctx   context.Context
	stop  context.CancelFunc
}

// Runtime is a fake runtime.
type Runtime struct {
	// GuestStateDir is the directory for guest read snapshots.
	GuestStateDir string
	Logger        *slog.Logger

	mu        sync.Mutex
	vms       map[string]*vm
	snapshots map[string]runtime.SnapshotInfo
	// CreateErr, StartErr make the next Create or Start fail.
	CreateErr error
	StartErr  error
	// StartGate, when set, blocks Start until it is closed or ctx ends.
	StartGate chan struct{}
	// Calls records the operations in order, for example "create:<id>".
	Calls []string
	// ClockSets records the guest clock values that the agent set.
	ClockSets []time.Time
}

// New returns a fake runtime.
func New(guestStateDir string) *Runtime {
	return &Runtime{
		GuestStateDir: guestStateDir,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		vms:           map[string]*vm{},
		snapshots:     map[string]runtime.SnapshotInfo{},
	}
}

var _ runtime.Runtime = (*Runtime)(nil)

func (r *Runtime) record(call string) {
	r.Calls = append(r.Calls, call)
}

// Name returns "fake".
func (r *Runtime) Name() string { return "fake" }

// Capabilities reports a Linux guest with a built LINUX base image.
func (r *Runtime) Capabilities(context.Context) runtime.Capabilities {
	return runtime.Capabilities{
		GuestOSes:        []ownsandv1.GuestOs{ownsandv1.GuestOs_GUEST_OS_LINUX},
		BuiltBaseImages:  []ownsandv1.BaseImage{ownsandv1.BaseImage_BASE_IMAGE_LINUX},
		HypervisorChecks: map[string]string{"fake": "ok"},
	}
}

// Create records a VM.
func (r *Runtime) Create(_ context.Context, spec runtime.VMSpec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record("create:" + spec.SandboxID)
	if err := r.CreateErr; err != nil {
		r.CreateErr = nil
		return err
	}
	if _, ok := r.vms[spec.SandboxID]; ok {
		return errors.New("vm already exists")
	}
	r.vms[spec.SandboxID] = &vm{spec: spec, state: VMCreated}
	return nil
}

// Start starts the in-process guest helper of the VM.
func (r *Runtime) Start(ctx context.Context, id string) (ownsandv1.RestorePath, error) {
	r.mu.Lock()
	gate := r.StartGate
	r.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record("start:" + id)
	if err := r.StartErr; err != nil {
		r.StartErr = nil
		return 0, err
	}
	v, ok := r.vms[id]
	if !ok {
		return 0, runtime.ErrVMNotFound
	}
	v.ctx, v.stop = context.WithCancel(context.Background())
	v.guest = guest.NewServer(guest.Config{StateDir: r.GuestStateDir, Logger: r.Logger, SetClock: r.setClock})
	v.state = VMRunning
	if v.spec.Source.SnapshotID != "" {
		if v.spec.TryMemoryRestore {
			return ownsandv1.RestorePath_RESTORE_PATH_MEMORY_STATE, nil
		}
		return ownsandv1.RestorePath_RESTORE_PATH_COLD_BOOT, nil
	}
	return ownsandv1.RestorePath_RESTORE_PATH_BASE_IMAGE, nil
}

func (r *Runtime) setClock(t time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ClockSets = append(r.ClockSets, t)
	return nil
}

func (r *Runtime) transition(id, call string, from []VMState, to VMState) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record(call + ":" + id)
	v, ok := r.vms[id]
	if !ok {
		return runtime.ErrVMNotFound
	}
	if !slices.Contains(from, v.state) {
		return fmt.Errorf("cannot %s a VM in state %d", call, v.state)
	}
	v.state = to
	return nil
}

// Pause marks the VM paused.
func (r *Runtime) Pause(_ context.Context, id string) error {
	return r.transition(id, "pause", []VMState{VMRunning}, VMPaused)
}

// Resume marks the VM running.
func (r *Runtime) Resume(_ context.Context, id string) error {
	return r.transition(id, "resume", []VMState{VMPaused}, VMRunning)
}

// Stop stops the guest helper.
func (r *Runtime) Stop(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record("stop:" + id)
	if v, ok := r.vms[id]; ok {
		if v.stop != nil {
			v.stop()
		}
		v.state = VMStopped
	}
	return nil
}

// Delete forgets the VM.
func (r *Runtime) Delete(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record("delete:" + id)
	if v, ok := r.vms[id]; ok {
		if v.stop != nil {
			v.stop()
		}
		delete(r.vms, id)
	}
	return nil
}

// Snapshot records a snapshot of a running or paused VM.
func (r *Runtime) Snapshot(_ context.Context, id, snapshotID string) (runtime.SnapshotInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record("snapshot:" + id)
	v, ok := r.vms[id]
	if !ok || (v.state != VMRunning && v.state != VMPaused) {
		return runtime.SnapshotInfo{}, runtime.ErrVMNotFound
	}
	info := runtime.SnapshotInfo{SnapshotID: snapshotID, SizeBytes: v.spec.DiskGiB << 30, HasMemoryState: true}
	r.snapshots[snapshotID] = info
	return info, nil
}

// DeleteSnapshot forgets a snapshot.
func (r *Runtime) DeleteSnapshot(_ context.Context, snapshotID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.snapshots, snapshotID)
	return nil
}

// List returns the running and paused VMs.
func (r *Runtime) List(context.Context) ([]runtime.VMInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []runtime.VMInfo
	for id, v := range r.vms {
		if v.state == VMRunning || v.state == VMPaused {
			out = append(out, runtime.VMInfo{SandboxID: id, Paused: v.state == VMPaused})
		}
	}
	return out, nil
}

// DialGuest connects to the in-process guest helper over net.Pipe.
func (r *Runtime) DialGuest(_ context.Context, id string) (io.ReadWriteCloser, error) {
	r.mu.Lock()
	v, ok := r.vms[id]
	r.mu.Unlock()
	if !ok || v.guest == nil || v.state == VMStopped {
		return nil, runtime.ErrVMNotFound
	}
	server, client := net.Pipe()
	go func() { _ = v.guest.ServeConn(v.ctx, server) }()
	return client, nil
}

// DeleteAll forgets every VM and snapshot.
func (r *Runtime) DeleteAll(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record("delete-all")
	for id, v := range r.vms {
		if v.stop != nil {
			v.stop()
		}
		delete(r.vms, id)
	}
	r.snapshots = map[string]runtime.SnapshotInfo{}
	return nil
}

// Close stops every guest helper.
func (r *Runtime) Close() error { return r.DeleteAll(context.Background()) }

// SimulateLoss removes a VM as if the host lost it.
func (r *Runtime) SimulateLoss(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v, ok := r.vms[id]; ok {
		if v.stop != nil {
			v.stop()
		}
		delete(r.vms, id)
	}
}

// AddRunningVM adds a VM that the runtime runs without a record, as after a
// lost records file.
func (r *Runtime) AddRunningVM(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.vms[id] = &vm{spec: runtime.VMSpec{SandboxID: id}, state: VMRunning}
}

// HasVM reports whether the VM exists.
func (r *Runtime) HasVM(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.vms[id]
	return ok
}

// CallLog returns a copy of the recorded calls.
func (r *Runtime) CallLog() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.Calls)
}

// ClockSetCount returns how often the guest clock was set.
func (r *Runtime) ClockSetCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.ClockSets)
}

// WaitClockSets waits until the guest clock was set at least n times.
func (r *Runtime) WaitClockSets(n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if r.ClockSetCount() >= n {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}
