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

// stopTimeout bounds the wait for a killed helper to be reaped.
const stopTimeout = 10 * time.Second

// errHelperExited reports that the guest helper of a sandbox was reaped. The
// operating system may have given its process ID, and so its process group ID,
// to an unrelated process, so the runtime sends no signal to that group.
var errHelperExited = errors.New("processrt: guest helper has exited")

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
// PATH. A relative guestBinary resolves against the working directory of the
// agent at this call; the runtime stores the absolute path because Create
// starts the helper with the sandbox root as its working directory.
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
	absBinary, err := filepath.Abs(guestBinary)
	if err != nil {
		return nil, fmt.Errorf("processrt: resolve guest helper binary %s: %w", guestBinary, err)
	}
	guestBinary = absBinary
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

// HostGuest returns the guest OS and the base image of the sandboxes that
// the runtime runs on this host: the host OS and its base image, because the
// guest helper runs as a host process. They are the only guest OS and base
// image that Capabilities reports, and Create rejects other guest OSes.
func HostGuest() (ownsandv1.GuestOs, ownsandv1.BaseImage) { return hostGuestOS, hostBaseImage }

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
	id  string
	dir string
	// socket is the listen address of the guest helper, and socketDir is the
	// directory that holds it outside dir, or "" when the address needs no
	// directory (see newSocket).
	socket    string
	socketDir string
	cmd       *exec.Cmd
	exited    chan struct{}
	restore   ownsandv1.SandboxRestorePath
}

func (v *vm) SandboxID() string                         { return v.id }
func (v *vm) RestorePath() ownsandv1.SandboxRestorePath { return v.restore }
func (v *vm) Exited() <-chan struct{}                   { return v.exited }
func (v *vm) Dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", v.socket)
}

// hasExited reports whether the helper was reaped.
func (v *vm) hasExited() bool {
	select {
	case <-v.exited:
		return true
	default:
		return false
	}
}

// signalHelper sends sig to the helper's process group. It returns
// errHelperExited without a syscall when the helper was reaped: Cmd.Wait does
// not clear cmd.Process, so the negative process ID of a reaped helper can
// address an unrelated process group that reused the ID. A helper that is
// reaped between the check and the signal is a remaining, very short window
// that the runtime cannot close, because it does not control when Cmd.Wait
// reaps the helper.
func (v *vm) signalHelper(sig syscall.Signal) error {
	if v.hasExited() {
		return errHelperExited
	}
	return signalGroup(v.cmd, sig)
}

// ignoreExited drops errHelperExited from err. A rollback that finds the
// helper gone has nothing left to undo for it.
func ignoreExited(err error) error {
	if errors.Is(err, errHelperExited) {
		return nil
	}
	return err
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
	socket, socketDir, err := newSocket(spec.SandboxID)
	if err != nil {
		os.RemoveAll(spec.Dir)
		return nil, err
	}
	v := &vm{id: spec.SandboxID, dir: spec.Dir, socket: socket, socketDir: socketDir, exited: make(chan struct{}), restore: restore}
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
		removeSocketDir(v)
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
			r.deleteAfterFailedStart(v)
			return nil, fmt.Errorf("processrt: guest helper not ready: %w", err)
		}
	case <-v.exited:
		r.deleteAfterFailedStart(v)
		return nil, errors.New("processrt: guest helper exited during start")
	}
	r.logger.Info("process sandbox started", "sandbox_id", spec.SandboxID, "pid", cmd.Process.Pid)
	return v, nil
}

// deleteAfterFailedStart removes a sandbox whose helper did not become ready.
//
// The caller records a failed create as terminal and does not delete the
// sandbox again, so this cleanup has no retry path through Delete and must not
// depend on the process listing that Delete uses to find exec workers. The
// helper was never handed out, so no caller could start an exec worker below
// it, and the cleanup kills the helper's process group directly. A cleanup
// error is logged and the sandbox stays tracked; Close kills a helper that is still alive.
func (r *Runtime) deleteAfterFailedStart(v *vm) {
	if !v.hasExited() {
		_ = v.signalHelper(sigKill)
		select {
		case <-v.exited:
		case <-time.After(stopTimeout):
			r.logger.Error("process sandbox cleanup after a failed start failed", "sandbox_id", v.id, "error", "helper did not stop")
			return
		}
	}
	if err := r.removeSandbox(v); err != nil {
		r.logger.Error("process sandbox cleanup after a failed start failed", "sandbox_id", v.id, "error", err)
	}
}

// removeSocketDir removes the socket directory of a sandbox, if it has one.
func removeSocketDir(v *vm) error {
	if v.socketDir == "" {
		return nil
	}
	return os.RemoveAll(v.socketDir)
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

// Pause stops the helper's process group and the process groups of the
// exec workers below the helper.
//
// The guest helper starts every exec in a process group of its own, so a
// signal to the helper's group does not reach the exec workers. Pause stops
// the helper's group first, so that the helper cannot start another worker
// while Pause enumerates the process tree, and then stops each worker group.
//
// A failed Pause rolls back: the caller keeps the sandbox in the running
// state when Pause fails, and a later Resume of that state does not reach the
// runtime, so a half-stopped sandbox would stay frozen. The rollback
// continues the helper and every worker group that Pause enumerated, and it
// does not use ctx, because ctx can be the reason that Pause failed.
func (r *Runtime) Pause(ctx context.Context, m vmruntime.VM) error {
	v, err := r.lookup(m.SandboxID())
	if err != nil {
		return err
	}
	if err := v.signalHelper(sigStop); err != nil {
		return fmt.Errorf("processrt: pause sandbox %s: %w", v.id, err)
	}
	groups, err := r.execGroups(ctx, v)
	if err == nil {
		err = r.signalGroups(v, groups, sigStop)
	}
	if err != nil {
		// groups is empty when the enumeration failed, so only the helper
		// was stopped. A group that was not stopped ignores SIGCONT.
		rollbackErr := errors.Join(r.signalGroups(v, groups, sigCont), ignoreExited(v.signalHelper(sigCont)))
		if rollbackErr != nil {
			r.logger.Error("process sandbox pause rollback failed; the sandbox may stay stopped", "sandbox_id", v.id, "error", rollbackErr)
		}
		return errors.Join(err, rollbackErr)
	}
	return nil
}

// Resume continues the process groups of the exec workers and then the
// helper's process group.
//
// A failed Resume keeps the sandbox stopped: the caller keeps the sandbox in
// the paused state when Resume fails, and a later Pause of that state does not
// reach the runtime, so a helper that ran again while its workers stayed
// stopped would be recorded as paused. The helper is still stopped from the
// Pause, so no worker starts while Resume enumerates the process tree. When
// the enumeration fails, Resume leaves the helper stopped. When a signal
// fails, Resume stops the groups that it continued again, and it leaves the
// helper stopped. A retry of Resume then continues the whole sandbox.
func (r *Runtime) Resume(ctx context.Context, m vmruntime.VM) error {
	v, err := r.lookup(m.SandboxID())
	if err != nil {
		return err
	}
	if v.hasExited() {
		// No signal reaches a reaped helper, and the workers below it are not
		// resumed on its behalf.
		return fmt.Errorf("processrt: resume sandbox %s: %w", v.id, errHelperExited)
	}
	groups, err := r.execGroups(ctx, v)
	if err != nil {
		return err
	}
	err = r.signalGroups(v, groups, sigCont)
	if err == nil {
		err = v.signalHelper(sigCont)
	}
	if err != nil {
		// SIGSTOP to a group that is still stopped changes nothing, so the
		// rollback can signal every enumerated group. SIGSTOP does not use ctx.
		rollbackErr := errors.Join(r.signalGroups(v, groups, sigStop), ignoreExited(v.signalHelper(sigStop)))
		if rollbackErr != nil {
			r.logger.Error("process sandbox resume rollback failed; the sandbox may run partly", "sandbox_id", v.id, "error", rollbackErr)
		}
		return errors.Join(err, rollbackErr)
	}
	return nil
}

// signalDescendants sends sig to the process group of every process below
// the helper, except the helper's own group.
func (r *Runtime) signalDescendants(ctx context.Context, v *vm, sig syscall.Signal) error {
	groups, err := r.execGroups(ctx, v)
	if err != nil {
		return err
	}
	return r.signalGroups(v, groups, sig)
}

// execGroups returns the process groups of the processes below the helper,
// except the helper's own group. It finds the processes through their parent
// links, so a worker that detached from the helper's process tree (a double
// fork that the init process adopted) is not found; see the process runtime
// entry in docs/cmds-ownsand-agent-foundation.md.
func (r *Runtime) execGroups(ctx context.Context, v *vm) ([]int, error) {
	if v.cmd == nil || v.cmd.Process == nil {
		return nil, nil
	}
	if v.hasExited() {
		// The helper was reaped, so its process ID may belong to another
		// process now.
		return nil, nil
	}
	groups, err := descendantGroups(ctx, v.cmd.Process.Pid)
	if err != nil {
		return nil, fmt.Errorf("processrt: list exec process groups of sandbox %s: %w", v.id, err)
	}
	return groups, nil
}

// signalGroups sends sig to each process group in groups and joins the
// errors. It continues after an error, so that one failure does not leave the
// other groups unsignaled.
func (r *Runtime) signalGroups(v *vm, groups []int, sig syscall.Signal) error {
	var errs []error
	for _, pgid := range groups {
		if err := signalGroupID(pgid, sig); err != nil {
			errs = append(errs, fmt.Errorf("processrt: signal exec process group of sandbox %s: %w", v.id, err))
		}
	}
	return errors.Join(errs...)
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

// Delete kills the helper's process group and the process groups of the
// exec workers below the helper, and removes the sandbox files.
//
// The runtime keeps the sandbox tracked until the helper is reaped and the
// sandbox directory is removed. The helper is the only handle to the exec
// workers below it, so a failure before the helper is killed (the process
// listing, a signal to a worker group) returns an error and leaves the helper
// alive and the sandbox tracked, and a retry of Delete completes the cleanup.
// A failure leaves the helper stopped, because a continued helper could start
// another worker, and a continued paused sandbox would run partly.
//
// The cleanup does not stop when ctx is canceled: a canceled caller is
// typically an agent that shuts down, and an enumeration that failed for that
// reason would orphan the workers. The process listing and the wait for the
// helper have timeouts of their own.
//
// A helper that was reaped already (it crashed or was killed from outside)
// receives no signal, because its process ID may belong to another process
// now; Delete only removes the sandbox directory then.
func (r *Runtime) Delete(ctx context.Context, sandboxID string) error {
	r.mu.Lock()
	v, ok := r.vms[sandboxID]
	r.mu.Unlock()
	if !ok {
		return nil
	}
	if !v.hasExited() {
		if err := r.stopHelper(ctx, v); err != nil {
			return err
		}
		r.logger.Info("process sandbox stopped", "sandbox_id", sandboxID)
	}
	return r.removeSandbox(v)
}

// removeSandbox removes the files of a sandbox whose helper was reaped,
// including its socket directory, and then stops tracking it. A failed
// removal keeps the sandbox tracked.
func (r *Runtime) removeSandbox(v *vm) error {
	if err := errors.Join(os.RemoveAll(v.dir), removeSocketDir(v)); err != nil {
		return err
	}
	r.mu.Lock()
	if r.vms[v.id] == v {
		delete(r.vms, v.id)
	}
	r.mu.Unlock()
	return nil
}

// stopHelper kills the exec workers below the helper and then the helper, and
// waits until the helper is reaped. It returns early without killing the
// helper when it cannot kill the workers.
func (r *Runtime) stopHelper(ctx context.Context, v *vm) error {
	// Stop the helper first so that it cannot start another worker while the
	// process tree is enumerated, then kill the workers and the helper. A stop
	// error must not keep the helper alive.
	_ = v.signalHelper(sigStop)
	// WithoutCancel: see Delete. The listing has its own timeout.
	if err := r.signalDescendants(context.WithoutCancel(ctx), v, sigKill); err != nil {
		r.logger.Error("process sandbox exec workers may keep running; delete can be retried", "sandbox_id", v.id, "error", err)
		return err
	}
	_ = v.signalHelper(sigCont)
	_ = v.signalHelper(sigKill)
	select {
	case <-v.exited:
		return nil
	case <-time.After(stopTimeout):
		return fmt.Errorf("processrt: sandbox %s did not stop", v.id)
	}
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
