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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/config"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/guestclient"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	"github.com/delinoio/mono/internal/fsutil"
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

// helperStopTimeout bounds the wait until the kernel has stopped the helper
// after the runtime sent it SIGSTOP, and helperStopPoll is the interval between
// two observations of the helper's state.
const (
	helperStopTimeout = 5 * time.Second
	helperStopPoll    = 10 * time.Millisecond
)

// errHelperExited reports that the guest helper of a sandbox was reaped. The
// operating system may have given its process ID, and so its process group ID,
// to an unrelated process, so the runtime sends no signal to that group.
var errHelperExited = errors.New("processrt: guest helper has exited")

// Runtime is the process test runtime.
type Runtime struct {
	guestBinary string
	// vmsDir holds the sandbox directories (<state>/vms), or "" when the
	// runtime does not know it. Delete uses it to remove the files of a
	// sandbox that the runtime does not track, for example one that an
	// earlier agent process created.
	vmsDir string
	logger *slog.Logger
	mu     sync.Mutex
	vms    map[string]*vm
}

// Options configures a process runtime.
type Options struct {
	// GuestBinary is the guest helper binary; see New.
	GuestBinary string
	// VMsDir holds the sandbox directories (<state>/vms). When it is set,
	// Delete removes the directory of a sandbox that the runtime does not
	// track, so the files of a sandbox that an earlier agent process left
	// behind are removed. An empty VMsDir keeps Delete a no-op for an
	// untracked sandbox.
	VMsDir string
	Logger *slog.Logger
}

var _ vmruntime.Runtime = (*Runtime)(nil)

// New returns a process runtime that runs guestBinary. An empty
// guestBinary means `ownsand-guest` next to the agent executable, then on
// PATH. A relative guestBinary resolves against the working directory of the
// agent at this call; the runtime stores the absolute path because Create
// starts the helper with the sandbox root as its working directory.
//
// A runtime that New returns does not know the sandbox directories, so its
// Delete cannot remove the files of an untracked sandbox; use NewWithOptions
// with VMsDir for that.
func New(guestBinary string, logger *slog.Logger) (*Runtime, error) {
	return NewWithOptions(Options{GuestBinary: guestBinary, Logger: logger})
}

// NewWithOptions returns a process runtime that opts configure; see New for
// the guest binary rules.
func NewWithOptions(opts Options) (*Runtime, error) {
	guestBinary, logger := opts.GuestBinary, opts.Logger
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
	vmsDir := opts.VMsDir
	if vmsDir != "" {
		absVMs, err := filepath.Abs(vmsDir)
		if err != nil {
			return nil, fmt.Errorf("processrt: resolve sandbox directories %s: %w", vmsDir, err)
		}
		vmsDir = absVMs
	}
	return &Runtime{guestBinary: guestBinary, vmsDir: vmsDir, logger: logger, vms: map[string]*vm{}}, nil
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

// MacOSHardwareModelSupported reports true for every model. The runtime boots
// no VM, so a macOS snapshot runs on a macOS host whatever hardware model it
// records. The agent asks only for macOS guests, which this runtime runs only
// on a macOS host.
func (r *Runtime) MacOSHardwareModelSupported([]byte) bool { return true }

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

// helperProcess keeps the identity and signal path of the guest helper. On
// macOS the agent waits on a separate supervisor, which owns the helper.
type helperProcess struct {
	pid    int
	signal func(syscall.Signal) error
}

type vm struct {
	id  string
	dir string
	// socket is the listen address of the guest helper, and socketDir is the
	// directory that holds it outside dir, or "" when the address needs no
	// directory (see newSocket).
	socket    string
	socketDir string
	helper    helperProcess
	exited    chan struct{}
	restore   ownsandv1.SandboxRestorePath
	// onSignal is a test seam: it is nil in production and records each
	// signal that signalHelper sends to the helper's group, so that a test
	// can assert the signal order that a race would otherwise hide.
	onSignal func(syscall.Signal)
	// observeStopped and stopWait are test seams: they are nil and zero in
	// production. observeStopped replaces the observation of the helper's
	// state (helperStopped), so that a test can separate the delivery of
	// SIGSTOP from the moment the runtime observes it. stopWait replaces
	// helperStopTimeout.
	observeStopped func(ctx context.Context, pid int) (bool, error)
	stopWait       time.Duration
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

// signalHelper sends sig to the helper's process group. A process ID may
// be reused after reaping, so a known exited helper receives no signal.
func (v *vm) signalHelper(sig syscall.Signal) error {
	if v.hasExited() {
		return errHelperExited
	}
	if v.onSignal != nil {
		v.onSignal(sig)
	}
	if v.helper.signal == nil {
		return nil
	}
	return v.helper.signal(sig)
}

// waitHelperStopped waits until the helper has been observed stopped, or has
// exited, after the caller sent it SIGSTOP.
//
// A signal that signalHelper sent is only queued when the call returns: the
// helper keeps running until the kernel delivers it, so it can still handle a
// request and start an exec worker. A process listing taken before the stop
// would miss that worker, and Pause would leave it running and Delete would
// leave it orphaned after the helper died. Waiting until the helper is
// stopped makes the listing complete, because a stopped helper starts no
// further worker.
//
// A helper that was reaped counts as stopped: it starts no worker, and the
// runtime sends no signal to it (see signalHelper). The wait ends with an
// error when ctx ends or the helper does not stop within helperStopTimeout.
// The last observation error is part of that error, so that a missing `ps`
// is not reported as a helper that ignored the signal.
func (v *vm) waitHelperStopped(ctx context.Context) error {
	if v.helper.pid == 0 {
		return nil
	}
	timeout := helperStopTimeout
	if v.stopWait > 0 {
		timeout = v.stopWait
	}
	observe := helperStopped
	if v.observeStopped != nil {
		observe = v.observeStopped
	}
	pid := v.helper.pid
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(helperStopPoll)
	defer ticker.Stop()
	var observeErr error
	for {
		// Check for an exit before every observation: the process ID of a
		// reaped helper can belong to another process.
		if v.hasExited() {
			return nil
		}
		stopped, err := observe(ctx, pid)
		if err == nil && stopped {
			return nil
		}
		observeErr = err
		select {
		case <-v.exited:
			return nil
		case <-ctx.Done():
			return fmt.Errorf("processrt: wait for the helper of sandbox %s to stop: %w", v.id, ctx.Err())
		case <-timer.C:
			if observeErr != nil {
				return fmt.Errorf("processrt: sandbox %s helper did not stop within %s: %w", v.id, timeout, observeErr)
			}
			return fmt.Errorf("processrt: sandbox %s helper did not stop within %s", v.id, timeout)
		case <-ticker.C:
		}
	}
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
	// The helper starts with the sandbox root as its working directory, so a
	// relative spec.Dir (a relative agent state directory) would resolve a
	// second time below the root for --state-dir: the helper records
	// (guest-state) would land inside the root that Snapshot copies and the
	// agent never looks for them there. Every later use takes the absolute
	// path.
	dir, err := filepath.Abs(spec.Dir)
	if err != nil {
		return nil, fmt.Errorf("processrt: resolve sandbox directory %s: %w", spec.Dir, err)
	}
	if err := fsutil.RemoveAll(dir); err != nil {
		return nil, err
	}
	root := filepath.Join(dir, rootDir)
	restore := ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_BASE_IMAGE
	if spec.Snapshot != nil {
		// The copy of a large snapshot disk can run for a long time, so it
		// stops with the context error once ctx is done; the partial copy is
		// removed with the sandbox directory and no helper starts.
		if err := fsutil.CopyTreeContext(ctx, filepath.Join(spec.Snapshot.Dir, diskDir), root); err != nil {
			fsutil.RemoveAll(dir)
			return nil, fmt.Errorf("processrt: restore snapshot disk: %w", err)
		}
		restore = ownsandv1.SandboxRestorePath_SANDBOX_RESTORE_PATH_DISK_COLD_BOOT
	} else if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	socket, socketDir, err := newSocket(spec.SandboxID)
	if err != nil {
		fsutil.RemoveAll(dir)
		return nil, err
	}
	v := &vm{id: spec.SandboxID, dir: dir, socket: socket, socketDir: socketDir, exited: make(chan struct{}), restore: restore}
	if err := recordSocketDir(dir, socketDir); err != nil {
		fsutil.RemoveAll(dir)
		removeSocketDir(v)
		return nil, err
	}
	// A restore that was canceled after the last copied entry, or while the
	// socket was prepared, must not start a helper. Only a restore checks
	// here: the readiness wait below reports a canceled start of any other
	// sandbox after the helper started and cleans it up.
	if spec.Snapshot != nil {
		if err := ctx.Err(); err != nil {
			fsutil.RemoveAll(dir)
			removeSocketDir(v)
			return nil, fmt.Errorf("processrt: restore snapshot canceled before the helper started: %w", err)
		}
	}
	cmd := exec.Command(r.guestBinary,
		"--listen", "unix://"+v.socket,
		"--state-dir", filepath.Join(dir, stateDir),
		"--clock", "ignore",
	)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "HOME="+root)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	helper, wait, err := startHelper(cmd)
	if err != nil {
		fsutil.RemoveAll(dir)
		removeSocketDir(v)
		return nil, fmt.Errorf("processrt: start guest helper: %w", err)
	}
	v.helper = helper
	go func() {
		wait()
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
	r.logger.Info("process sandbox started", "sandbox_id", spec.SandboxID, "pid", helper.pid)
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
// the helper's group first and waits until the helper is observed stopped, so
// that the helper cannot start another worker while Pause enumerates the
// process tree, and then stops each worker group.
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
	// SIGSTOP is only queued when signalHelper returns, so the helper can
	// still start a worker. Enumerate the process tree after the stop took
	// effect.
	var groups []int
	err = v.waitHelperStopped(ctx)
	if err == nil {
		groups, err = r.execGroups(ctx, v)
	}
	if err == nil {
		err = r.signalGroups(v, groups, sigStop)
	}
	if err != nil {
		// groups is empty when the wait or the enumeration failed, so only
		// the helper was stopped. A group that was not stopped ignores SIGCONT.
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
	if v.helper.pid == 0 {
		return nil, nil
	}
	if v.hasExited() {
		// The helper was reaped, so its process ID may belong to another
		// process now.
		return nil, nil
	}
	groups, err := descendantGroups(ctx, v.helper.pid)
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
		return r.removeUntracked(sandboxID)
	}
	if !v.hasExited() {
		if err := r.stopHelper(ctx, v); err != nil {
			return err
		}
		r.logger.Info("process sandbox stopped", "sandbox_id", sandboxID)
	}
	return r.removeSandbox(v)
}

// socketDirFile is the file in a sandbox directory that records the socket
// directory of the sandbox when that directory lies outside the sandbox
// directory (macOS; see newSocket). A later agent process reads it to remove
// the socket directory of a sandbox that it does not track.
const socketDirFile = "socket-dir"

// socketDirPrefix is the name prefix of the socket directories that newSocket
// creates. removeUntracked refuses to remove a recorded directory without it,
// so a damaged record cannot make Delete remove an unrelated directory.
const socketDirPrefix = "osb-"

// recordSocketDir records socketDir in the sandbox directory dir. It does
// nothing when socketDir is empty.
func recordSocketDir(dir, socketDir string) error {
	if socketDir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("processrt: create sandbox directory: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, socketDirFile), []byte(socketDir), 0o600); err != nil {
		return fmt.Errorf("processrt: record guest helper socket directory: %w", err)
	}
	return nil
}

// recordedSocketDir returns the socket directory that dir records, or "" when
// dir records none or an unexpected path.
func recordedSocketDir(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, socketDirFile))
	if err != nil {
		return ""
	}
	socketDir := filepath.Clean(string(data))
	if !filepath.IsAbs(socketDir) || !strings.HasPrefix(filepath.Base(socketDir), socketDirPrefix) {
		return ""
	}
	return socketDir
}

// removeUntracked removes the files of a sandbox that the runtime does not
// track: one that an earlier agent process created, or one that was deleted
// already. The helper of such a sandbox is not reachable through the runtime.
// Delete is idempotent, so a sandbox without files is not an error. It does
// nothing when the runtime does not know the sandbox directories.
//
// The socket directory goes first and the sandbox directory, which records
// it, last, so that a failed removal can be retried.
func (r *Runtime) removeUntracked(sandboxID string) error {
	if r.vmsDir == "" {
		return nil
	}
	// A sandbox ID is one path element: an ID that is empty, "." or ".." or
	// that holds a separator would address a directory outside vmsDir.
	if sandboxID == "" || sandboxID == "." || sandboxID == ".." ||
		strings.ContainsAny(sandboxID, `/\`) || filepath.Base(sandboxID) != sandboxID {
		return fmt.Errorf("processrt: sandbox ID %q is not a single path element", sandboxID)
	}
	dir := filepath.Join(r.vmsDir, sandboxID)
	if socketDir := recordedSocketDir(dir); socketDir != "" {
		if err := os.RemoveAll(socketDir); err != nil {
			return err
		}
	}
	if err := fsutil.RemoveAll(dir); err != nil {
		return err
	}
	r.logger.Info("process sandbox files removed", "sandbox_id", sandboxID)
	return nil
}

// removeSandbox removes the files of a sandbox whose helper was reaped,
// including its socket directory, and then stops tracking it. A failed
// removal keeps the sandbox tracked.
func (r *Runtime) removeSandbox(v *vm) error {
	if err := errors.Join(fsutil.RemoveAll(v.dir), removeSocketDir(v)); err != nil {
		return err
	}
	r.mu.Lock()
	if r.vms[v.id] == v {
		delete(r.vms, v.id)
	}
	r.mu.Unlock()
	return nil
}

// stopHelper stops the helper, kills the exec workers below it and then the
// helper, and waits until the helper is reaped. It returns early without
// killing the helper when the helper does not stop or it cannot kill the
// workers.
func (r *Runtime) stopHelper(ctx context.Context, v *vm) error {
	// Stop the helper first so that it cannot start another worker while the
	// process tree is enumerated, then kill the workers and the helper. A stop
	// error must not keep the helper alive.
	_ = v.signalHelper(sigStop)
	// WithoutCancel: see Delete. The wait and the listing have timeouts of
	// their own. The signal is only queued: enumerate the tree after the
	// helper is observed stopped, or a worker that the helper starts in
	// between would survive the kill. A helper that does not stop leaves the
	// helper alive and tracked, and a retry of Delete completes the cleanup.
	if err := v.waitHelperStopped(context.WithoutCancel(ctx)); err != nil {
		r.logger.Error("process sandbox helper did not stop; delete can be retried", "sandbox_id", v.id, "error", err)
		return err
	}
	if err := r.signalDescendants(context.WithoutCancel(ctx), v, sigKill); err != nil {
		r.logger.Error("process sandbox exec workers may keep running; delete can be retried", "sandbox_id", v.id, "error", err)
		return err
	}
	// The helper stays stopped until it is killed: SIGKILL ends a stopped
	// process without a SIGCONT, and a SIGCONT before it would let the helper
	// handle a request that it received before the stop and start a worker
	// below it after the enumeration above, an orphaned worker that nothing
	// kills.
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

// Close stops every helper and removes every tracked sandbox, including a
// sandbox whose helper exited already, so that its files do not outlive the
// agent process.
func (r *Runtime) Close() error {
	r.mu.Lock()
	ids := make([]string, 0, len(r.vms))
	for id := range r.vms {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	var errs []error
	for _, id := range ids {
		errs = append(errs, r.Delete(context.Background(), id))
	}
	return errors.Join(errs...)
}
