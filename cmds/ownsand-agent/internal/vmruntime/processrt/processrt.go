// SPDX-License-Identifier: FSL-1.1-ALv2

// Package processrt is the process test runtime. It runs the real guest
// helper binary as a host process for each sandbox, so end-to-end tests can
// run the agent without a hypervisor.
//
// It gives NO isolation: sandbox commands run on the host as the agent's
// user. The agent contract requires a VM for every sandbox; this runtime is
// an explicit test-only exception that an operator must select with
// OWNSAND_AGENT_RUNTIME=process, and it reports a failing
// `process-test-runtime` hypervisor check so that it is visible on the
// device.
package processrt

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/config"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/guestclient"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/launcher"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	"github.com/delinoio/mono/internal/fsutil"
	"github.com/delinoio/mono/internal/procgroup"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Directory names inside a sandbox directory and a snapshot directory.
const (
	rootDir  = "root"
	diskDir  = "disk"
	stateDir = "guest-state"
)

// ReadyTimeout bounds the wait for the guest helper to answer.
const ReadyTimeout = 30 * time.Second

// Runtime is the process test runtime.
type Runtime struct {
	guestBinary string
	logger      *slog.Logger
	mu          sync.Mutex
	vms         map[string]*vm
}

var _ vmruntime.Runtime = (*Runtime)(nil)

// New returns a process runtime that runs guestBinary. An empty
// guestBinary means `ownsand-guest` next to the agent executable, then on
// PATH.
func New(guestBinary string, logger *slog.Logger) (*Runtime, error) {
	if !supported {
		return nil, fmt.Errorf("%w: the process test runtime needs a Linux or macOS host", vmruntime.ErrUnsupported)
	}
	if guestBinary == "" {
		guestBinary = findGuestBinary()
	}
	if guestBinary == "" {
		return nil, errors.New("processrt: guest helper binary not found; set " + config.EnvGuestBinary)
	}
	if st, err := os.Stat(guestBinary); err != nil || st.IsDir() {
		return nil, fmt.Errorf("processrt: guest helper binary %s is not a file", guestBinary)
	}
	logger.Warn("process test runtime selected: sandboxes run as host processes without isolation")
	return &Runtime{guestBinary: guestBinary, logger: logger, vms: map[string]*vm{}}, nil
}

func findGuestBinary() string {
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "ownsand-guest")
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			return candidate
		}
	}
	if p, err := exec.LookPath("ownsand-guest"); err == nil {
		return p
	}
	return ""
}

// Kind returns RuntimeKindProcess.
func (r *Runtime) Kind() config.RuntimeKind { return config.RuntimeKindProcess }

// Capabilities reports the host OS as the only guest OS and its base image.
func (r *Runtime) Capabilities(context.Context) vmruntime.Capabilities {
	return vmruntime.Capabilities{
		GuestOSes:  []ownsandv1.GuestOs{hostGuestOS},
		BaseImages: []ownsandv1.BaseImage{hostBaseImage},
		Checks: []*ownsandv1.HypervisorCheck{{
			CheckId: "process-test-runtime",
			Passed:  false,
			Detail:  "test runtime: sandboxes run as host processes without isolation",
		}},
	}
}

type vm struct {
	id      string
	dir     string
	socket  string
	cmd     *exec.Cmd
	exited  chan struct{}
	restore ownsandv1.SandboxRestorePath
}

func (v *vm) SandboxID() string                         { return v.id }
func (v *vm) RestorePath() ownsandv1.SandboxRestorePath { return v.restore }
func (v *vm) Exited() <-chan struct{}                   { return v.exited }
func (v *vm) Dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", v.socket)
}

// Create starts the guest helper for the sandbox.
func (r *Runtime) Create(ctx context.Context, spec vmruntime.Spec) (vmruntime.VM, error) {
	if spec.GuestOS != hostGuestOS {
		return nil, fmt.Errorf("%w: the process runtime runs only %s guests", vmruntime.ErrUnsupported, hostGuestOS)
	}
	r.mu.Lock()
	if _, ok := r.vms[spec.SandboxID]; ok {
		r.mu.Unlock()
		return nil, fmt.Errorf("processrt: sandbox %s already runs", spec.SandboxID)
	}
	r.mu.Unlock()
	if err := os.RemoveAll(spec.Dir); err != nil {
		return nil, err
	}
	root := filepath.Join(spec.Dir, rootDir)
	restore := ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_BASE_IMAGE
	if spec.Snapshot != nil {
		if err := fsutil.CopyTree(filepath.Join(spec.Snapshot.Dir, diskDir), root); err != nil {
			os.RemoveAll(spec.Dir)
			return nil, fmt.Errorf("processrt: restore snapshot disk: %w", err)
		}
		restore = ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_DISK_COLD_BOOT
	} else if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	v := &vm{id: spec.SandboxID, dir: spec.Dir, socket: socketPath(spec.Dir, spec.SandboxID), exited: make(chan struct{}), restore: restore}
	cmd := exec.Command(r.guestBinary,
		"--listen", "unix://"+v.socket,
		"--state-dir", filepath.Join(spec.Dir, stateDir),
		"--clock", "ignore",
	)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "HOME="+root)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	procgroup.Set(cmd)
	if err := launcher.Start(cmd); err != nil {
		os.RemoveAll(spec.Dir)
		return nil, fmt.Errorf("processrt: start guest helper: %w", err)
	}
	v.cmd = cmd
	go func() {
		_ = cmd.Wait()
		close(v.exited)
	}()
	r.mu.Lock()
	r.vms[spec.SandboxID] = v
	r.mu.Unlock()

	readyCtx, cancel := context.WithTimeout(ctx, ReadyTimeout)
	defer cancel()
	client := guestclient.New(v.Dial)
	defer client.Close()
	readyDone := make(chan error, 1)
	go func() {
		_, err := client.WaitReady(readyCtx, 50*time.Millisecond)
		readyDone <- err
	}()
	select {
	case err := <-readyDone:
		if err != nil {
			r.Delete(context.Background(), spec.SandboxID)
			return nil, fmt.Errorf("processrt: guest helper not ready: %w", err)
		}
	case <-v.exited:
		r.Delete(context.Background(), spec.SandboxID)
		return nil, errors.New("processrt: guest helper exited during start")
	}
	r.logger.Info("process sandbox started", "sandbox_id", spec.SandboxID, "pid", cmd.Process.Pid)
	return v, nil
}

func (r *Runtime) lookup(id string) (*vm, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.vms[id]
	if !ok {
		return nil, fmt.Errorf("processrt: sandbox %s does not run", id)
	}
	return v, nil
}

// Pause stops the helper's process group.
func (r *Runtime) Pause(_ context.Context, m vmruntime.VM) error {
	v, err := r.lookup(m.SandboxID())
	if err != nil {
		return err
	}
	return signalGroup(v.cmd, sigStop)
}

// Resume continues the helper's process group.
func (r *Runtime) Resume(_ context.Context, m vmruntime.VM) error {
	v, err := r.lookup(m.SandboxID())
	if err != nil {
		return err
	}
	return signalGroup(v.cmd, sigCont)
}

// Snapshot copies the sandbox root directory as its disk. The process
// runtime has no memory state.
func (r *Runtime) Snapshot(_ context.Context, m vmruntime.VM, dir string) (vmruntime.SnapshotInfo, error) {
	v, err := r.lookup(m.SandboxID())
	if err != nil {
		return vmruntime.SnapshotInfo{}, err
	}
	if err := fsutil.CopyTree(filepath.Join(v.dir, rootDir), filepath.Join(dir, diskDir)); err != nil {
		return vmruntime.SnapshotInfo{}, fmt.Errorf("processrt: snapshot disk: %w", err)
	}
	size, err := fsutil.TreeSize(dir)
	if err != nil {
		return vmruntime.SnapshotInfo{}, err
	}
	return vmruntime.SnapshotInfo{SizeBytes: size}, nil
}

// Delete kills the helper's process group and removes the sandbox files.
func (r *Runtime) Delete(_ context.Context, sandboxID string) error {
	r.mu.Lock()
	v, ok := r.vms[sandboxID]
	delete(r.vms, sandboxID)
	r.mu.Unlock()
	if ok {
		_ = signalGroup(v.cmd, sigCont)
		_ = signalGroup(v.cmd, sigKill)
		select {
		case <-v.exited:
		case <-time.After(10 * time.Second):
			return fmt.Errorf("processrt: sandbox %s did not stop", sandboxID)
		}
		r.logger.Info("process sandbox stopped", "sandbox_id", sandboxID)
		return os.RemoveAll(v.dir)
	}
	return nil
}

// Running lists the sandboxes whose helper runs.
func (r *Runtime) Running(context.Context) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []string
	for id, v := range r.vms {
		select {
		case <-v.exited:
		default:
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// Close stops every helper.
func (r *Runtime) Close() error {
	ids, _ := r.Running(context.Background())
	var errs []error
	for _, id := range ids {
		errs = append(errs, r.Delete(context.Background(), id))
	}
	return errors.Join(errs...)
}

// signalGroup signals the process group of cmd that procgroup.Set created.
// A command that has not started is a no-op.
func signalGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return procgroup.Signal(cmd.Process, sig)
}
