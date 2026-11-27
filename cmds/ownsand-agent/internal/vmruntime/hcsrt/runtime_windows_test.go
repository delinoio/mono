// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"math/bits"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/images"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// failOpenSystem makes openSystemFunc return err for the test.
func failOpenSystem(t *testing.T, err error) {
	t.Helper()
	orig := openSystemFunc
	openSystemFunc = func(string) (*system, error) { return nil, err }
	t.Cleanup(func() { openSystemFunc = orig })
}

// newDeleteTestRuntime returns a runtime without a Hyper-V VM and a VM
// directory with a file, and skips the test where the HCS DLL does not load,
// because Delete then skips the orphan stop.
func newDeleteTestRuntime(t *testing.T, sandboxID string) (*Runtime, string) {
	t.Helper()
	if err := hcsLoad(); err != nil {
		t.Skipf("the HCS is not available: %v", err)
	}
	r := &Runtime{
		opts:   Options{VMsDir: t.TempDir()},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		vms:    map[string]*vm{},
	}
	dir := r.vmDir(sandboxID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "disk.vhd"), []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	return r, dir
}

func TestStopOrphanOpenOutcome(t *testing.T) {
	transient := &hcsError{op: "HcsOpenComputeSystem", hr: 0x80070005}
	for _, tc := range []struct {
		name    string
		openErr error
		wantErr error
	}{
		{"not found", &hcsError{op: "HcsOpenComputeSystem", hr: hrSystemNotFound}, nil},
		{"already stopped", &hcsError{op: "HcsOpenComputeSystem", hr: hrSystemAlreadyStopped}, nil},
		{"transient open failure", transient, transient},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failOpenSystem(t, tc.openErr)
			r := &Runtime{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			err := r.stopOrphan("sb")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("stopOrphan = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestDeleteKeepsVMDirWhenOrphanStopOutcomeUnknown(t *testing.T) {
	r, dir := newDeleteTestRuntime(t, "sb")
	transient := &hcsError{op: "HcsOpenComputeSystem", hr: 0x80070005}
	failOpenSystem(t, transient)
	err := r.Delete(t.Context(), "sb")
	if !errors.Is(err, transient) {
		t.Fatalf("Delete = %v, want the open error", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "disk.vhd")); serr != nil {
		t.Errorf("the VM directory content was removed: %v", serr)
	}
}

func TestDeleteRemovesVMDirWhenSystemAbsent(t *testing.T) {
	for _, hr := range []uint32{hrSystemNotFound, hrSystemAlreadyStopped} {
		r, dir := newDeleteTestRuntime(t, "sb")
		failOpenSystem(t, &hcsError{op: "HcsOpenComputeSystem", hr: hr})
		if err := r.Delete(t.Context(), "sb"); err != nil {
			t.Fatalf("Delete with hr %#x = %v", hr, err)
		}
		if _, serr := os.Stat(dir); !os.IsNotExist(serr) {
			t.Errorf("hr %#x: the VM directory still exists: %v", hr, serr)
		}
	}
}

// fakeSystem returns a compute system without an HCS handle. terminate runs
// in place of the HCS terminate call and can mark the system as exited, like
// the exit callback of the HCS does.
func fakeSystem(terminate func(s *system) error) *system {
	s := newSystem("fake", 0)
	s.terminateFn = func() error { return terminate(s) }
	return s
}

func registerFakeCallback(t *testing.T, s *system) {
	t.Helper()
	callbackMu.Lock()
	callbackNext++
	s.cbKey = callbackNext
	callbackSys[s.cbKey] = s
	callbackMu.Unlock()
	t.Cleanup(s.close)
}

func notifySystem(s *system, event uint32) { systemCallback(&hcsEvent{Type: event}, s.cbKey) }

func waitForSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s signal stayed open", name)
	}
}

func requireOpenSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
		t.Fatalf("%s signal closed unexpectedly", name)
	default:
	}
}

// shortStopTimeout shortens the wait for an exit event for the test.
func shortStopTimeout(t *testing.T) {
	t.Helper()
	orig := stopSystemTimeout
	stopSystemTimeout = 50 * time.Millisecond
	t.Cleanup(func() { stopSystemTimeout = orig })
}

// openFakeSystem makes openSystemFunc return the system that next returns for
// the test.
func openFakeSystem(t *testing.T, next func() (*system, error)) {
	t.Helper()
	orig := openSystemFunc
	openSystemFunc = func(string) (*system, error) { return next() }
	t.Cleanup(func() { openSystemFunc = orig })
}

func exitsOnTerminate(s *system) error { s.markExited(); return nil }

func neverExits(*system) error { return nil }

func TestStopSystemNeedsConfirmedExit(t *testing.T) {
	shortStopTimeout(t)
	termErr := errors.New("terminate failed")
	statusErr := errors.New("status unavailable")
	disconnectDuringTerminate := func(s *system) error {
		notifySystem(s, hcsEventServiceDisconnect)
		return termErr
	}
	for _, tc := range []struct {
		name      string
		terminate func(s *system) error
		verify    func() (bool, error)
		wantErr   error
		wantTerm  bool
	}{
		{"terminate and exit event", exitsOnTerminate, nil, nil, false},
		{"terminate error but exit event", func(s *system) error { notifySystem(s, hcsEventSystemExited); return termErr }, nil, nil, false},
		{"terminate without exit event", neverExits, nil, errStopUnconfirmed, false},
		{"terminate error without exit event", func(*system) error { return termErr }, nil, errStopUnconfirmed, true},
		{"terminate reports already stopped", func(*system) error { return &hcsError{hr: hrSystemAlreadyStopped} }, nil, nil, false},
		{"disconnect with unknown status", disconnectDuringTerminate, func() (bool, error) { return false, statusErr }, errStopUnconfirmed, true},
		{"disconnect with running VM", disconnectDuringTerminate, func() (bool, error) { return false, nil }, errStopUnconfirmed, true},
		{"disconnect with confirmed exit", disconnectDuringTerminate, func() (bool, error) { return true, nil }, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &Runtime{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			sys := fakeSystem(tc.terminate)
			registerFakeCallback(t, sys)
			if tc.verify != nil {
				sys.verifyExitFn = func(string, time.Time) (bool, error) { return tc.verify() }
			}
			err := r.stopSystem("sb", sys)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("stopSystem = %v, want %v", err, tc.wantErr)
			}
			if tc.wantTerm && !errors.Is(err, termErr) {
				t.Errorf("stopSystem = %v, want it to wrap the terminate error", err)
			}
			waitForSignal(t, sys.closed, "handle closed")
			select {
			case <-sys.exited:
				if tc.wantErr != nil {
					t.Error("an unconfirmed stop marked the system as exited")
				}
			default:
				if tc.wantErr == nil {
					t.Error("a confirmed stop left the system not exited")
				}
			}
		})
	}
}

func TestStopOrphanUnconfirmedExit(t *testing.T) {
	shortStopTimeout(t)
	openFakeSystem(t, func() (*system, error) { return fakeSystem(neverExits), nil })
	r := &Runtime{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := r.stopOrphan("sb"); !errors.Is(err, errStopUnconfirmed) {
		t.Fatalf("stopOrphan = %v, want errStopUnconfirmed", err)
	}
}

func TestDeleteKeepsVMDirWhenOrphanExitUnconfirmed(t *testing.T) {
	shortStopTimeout(t)
	r, dir := newDeleteTestRuntime(t, "sb")
	terminate := neverExits
	openFakeSystem(t, func() (*system, error) { return fakeSystem(terminate), nil })
	if err := r.Delete(t.Context(), "sb"); !errors.Is(err, errStopUnconfirmed) {
		t.Fatalf("Delete = %v, want errStopUnconfirmed", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "disk.vhd")); serr != nil {
		t.Errorf("the VM directory content was removed: %v", serr)
	}
	// A retry that confirms the exit finishes the delete.
	terminate = exitsOnTerminate
	if err := r.Delete(t.Context(), "sb"); err != nil {
		t.Fatalf("retry Delete = %v", err)
	}
	if _, serr := os.Stat(dir); !os.IsNotExist(serr) {
		t.Errorf("the VM directory still exists after the confirmed stop: %v", serr)
	}
}

func TestDeleteKeepsTrackedVMWhenExitUnconfirmed(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		name := "no exit event"
		if disconnect {
			name = "disconnect and VM still running"
		}
		t.Run(name, func(t *testing.T) { testDeleteKeepsTrackedVM(t, disconnect) })
	}
}

func testDeleteKeepsTrackedVM(t *testing.T, disconnect bool) {
	t.Helper()
	shortStopTimeout(t)
	r, dir := newDeleteTestRuntime(t, "sb")
	sys := fakeSystem(func(s *system) error {
		if disconnect {
			notifySystem(s, hcsEventServiceDisconnect)
		}
		return nil
	})
	registerFakeCallback(t, sys)
	sys.verifyExitFn = func(string, time.Time) (bool, error) { return false, nil }
	v := &vm{id: "sb", dir: dir, exited: make(chan struct{}), stopping: make(chan struct{})}
	v.attachSystem(sys)
	watchDone := v.watchDone
	r.vms["sb"] = v
	if err := r.Delete(t.Context(), "sb"); !errors.Is(err, errStopUnconfirmed) {
		t.Fatalf("Delete = %v, want errStopUnconfirmed", err)
	}
	if r.vms["sb"] != v {
		t.Error("Delete dropped the registration of a VM whose exit is not confirmed")
	}
	waitForSignal(t, watchDone, "exit watcher")
	waitForSignal(t, sys.closed, "handle closed")
	select {
	case <-v.exited:
		t.Error("the VM counts as exited without a confirmed exit")
	default:
	}
	if _, serr := os.Stat(filepath.Join(dir, "disk.vhd")); serr != nil {
		t.Errorf("the VM directory content was removed: %v", serr)
	}
	// The handle is closed, so the retry reopens the system by its ID. A
	// system that is gone confirms the stop.
	failOpenSystem(t, &hcsError{op: "HcsOpenComputeSystem", hr: hrSystemNotFound})
	if err := r.Delete(t.Context(), "sb"); err != nil {
		t.Fatalf("retry Delete = %v", err)
	}
	if _, ok := r.vms["sb"]; ok {
		t.Error("the registration remains after a confirmed stop")
	}
	select {
	case <-v.exited:
	default:
		t.Error("the VM is not exited after a confirmed stop")
	}
	if _, serr := os.Stat(dir); !os.IsNotExist(serr) {
		t.Errorf("the VM directory still exists after the confirmed stop: %v", serr)
	}
	if err := r.Delete(t.Context(), "sb"); err != nil {
		t.Fatalf("repeated Delete = %v", err)
	}
}

func TestVMWatcherForwardsConfirmedExit(t *testing.T) {
	for _, tc := range []string{"active handle", "closing handle", "disconnect and running status"} {
		t.Run(tc, func(t *testing.T) {
			sys := fakeSystem(neverExits)
			registerFakeCallback(t, sys)
			v := &vm{exited: make(chan struct{})}
			switch tc {
			case "active handle":
				v.attachSystem(sys)
			case "closing handle":
				notifySystem(sys, hcsEventSystemExited)
				sys.close()
				v.attachSystem(sys)
			case "disconnect and running status":
				queried := make(chan struct{})
				sys.verifyExitFn = func(string, time.Time) (bool, error) {
					close(queried)
					return false, nil
				}
				v.attachSystem(sys)
				notifySystem(sys, hcsEventServiceDisconnect)
				notifySystem(sys, hcsEventServiceDisconnect)
				waitForSignal(t, queried, "status query")
				requireOpenSignal(t, v.Exited(), "VM exit")
				requireOpenSignal(t, v.watchDone, "exit watcher")
			}
			notifySystem(sys, hcsEventSystemExited)
			waitForSignal(t, v.Exited(), "VM exit")
			waitForSignal(t, v.watchDone, "exit watcher")
			sys.close()
			sys.close()
		})
	}
}

func TestFailedCreateReleasesWatcherAndKeepsUnconfirmedVM(t *testing.T) {
	shortStopTimeout(t)
	const id = "failed-create"
	r, dir := newDeleteTestRuntime(t, id)
	r.opts.Kernel = filepath.Join(r.opts.VMsDir, "kernel")
	if err := os.WriteFile(r.opts.Kernel, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	failOpenSystem(t, &hcsError{op: "HcsOpenComputeSystem", hr: hrSystemNotFound})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sys := fakeSystem(neverExits)
	registerFakeCallback(t, sys)
	spec := vmruntime.Spec{
		SandboxID: id,
		GuestOS:   ownsandv1.GuestOs_GUEST_OS_LINUX,
		Dir:       dir,
		Resources: &ownsandv1.Resources{DiskGib: 1},
	}
	ops := vmCreateOps{
		copyFile: func(_ context.Context, _, dst string) error {
			return os.WriteFile(dst, []byte("disk"), 0o600)
		},
		makeFixedVHD: func(_ string, size int64, _ bool) (int64, error) { return size, nil },
		grantVM: func(computeID, path string) error {
			if computeID != systemID(id) || path != filepath.Join(dir, diskFile) {
				t.Fatalf("grantVM(%q, %q): unexpected compute system ID or disk path", computeID, path)
			}
			return nil
		},
		createSystem: func(string, any) (*system, error) {
			cancel()
			return sys, nil
		},
	}
	if v, err := r.createVM(ctx, spec, ops); v != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("createVM = %v, %v, want nil, context.Canceled", v, err)
	}
	v := r.vms[id]
	if v == nil {
		t.Fatal("failed-create cleanup dropped the VM registration")
	}
	waitForSignal(t, v.watchDone, "exit watcher")
	waitForSignal(t, sys.closed, "handle closed")
	requireOpenSignal(t, v.Exited(), "VM exit")
	if _, err := os.Stat(filepath.Join(dir, diskFile)); err != nil {
		t.Fatalf("failed-create cleanup removed the VM disk: %v", err)
	}
	if err := r.Delete(t.Context(), id); err != nil {
		t.Fatalf("retry Delete = %v", err)
	}
	waitForSignal(t, v.Exited(), "VM exit")
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) || r.vms[id] != nil {
		t.Fatalf("the VM registration or directory remains after retry: %v", err)
	}
}

func TestDeleteRemovesTrackedVMWhenExitConfirmed(t *testing.T) {
	r, dir := newDeleteTestRuntime(t, "sb")
	v := &vm{id: "sb", dir: dir, exited: make(chan struct{}), stopping: make(chan struct{}),
		sys: fakeSystem(exitsOnTerminate)}
	r.vms["sb"] = v
	if err := r.Delete(t.Context(), "sb"); err != nil {
		t.Fatalf("Delete = %v", err)
	}
	if _, ok := r.vms["sb"]; ok {
		t.Error("the registration remains after a confirmed stop")
	}
	if _, serr := os.Stat(dir); !os.IsNotExist(serr) {
		t.Errorf("the VM directory still exists: %v", serr)
	}
}

// TestCopyFileEndsWithItsContext checks that copyFile copies the whole file on
// a live context, refuses a context that already ended, and removes the
// partial destination of a copy that a context cancellation interrupted.
func TestCopyFileEndsWithItsContext(t *testing.T) {
	t.Run("live", func(t *testing.T) {
		dir := t.TempDir()
		src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
		want := []byte("source contents")
		if err := os.WriteFile(src, want, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := copyFile(t.Context(), src, dst); err != nil {
			t.Fatalf("copy on a live context = %v", err)
		}
		if got, err := os.ReadFile(dst); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("destination = %q, %v, want %q", got, err, want)
		}
	})
	t.Run("already_canceled", func(t *testing.T) {
		dir := t.TempDir()
		src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
		if err := os.WriteFile(src, []byte("source contents"), 0o600); err != nil {
			t.Fatal(err)
		}
		want := []byte("existing destination")
		if err := os.WriteFile(dst, want, 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := copyFile(ctx, src, dst); !errors.Is(err, context.Canceled) {
			t.Fatalf("copy on a canceled context = %v, want context.Canceled", err)
		}
		if got, err := os.ReadFile(dst); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("destination = %q, %v, want %q", got, err, want)
		}
	})
	t.Run("in_flight", func(t *testing.T) {
		const size = 64 << 20
		dir := t.TempDir()
		src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
		if err := os.WriteFile(src, bytes.Repeat([]byte{7}, size), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		progress := &copyProgressFixture{ctx: ctx, cancel: cancel}
		err := copyFileWithProgress(ctx, src, dst, progress.attach)
		if progress.transferred <= 0 || progress.transferred >= progress.total || progress.total != size {
			t.Fatalf("CopyFileExW reported no strictly partial progress for the %d-byte source (observed %d of %d, copy error: %v)", size, progress.transferred, progress.total, err)
		}
		if progress.err != nil {
			t.Fatal(progress.err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled copy = %v, want context.Canceled", err)
		}
		if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat of partial destination = %v, want os.ErrNotExist", err)
		}
		t.Logf("CopyFileExW reported %d of %d bytes with a live context, then aborted with pbCancel set and removed the destination", progress.transferred, progress.total)
	})
}

type copyProgressFixture struct {
	ctx         context.Context
	cancel      context.CancelFunc
	flag        *int32
	watcherDone <-chan struct{}
	transferred int64
	total       int64
	err         error
}

// NewCallback retains the trampoline for the process lifetime. The native
// call receives separate fixture data for each copy through lpData.
var copyProgressRoutine = sync.OnceValue(func() uintptr {
	if bits.UintSize == 32 {
		// On windows/386 each LARGE_INTEGER occupies two stack slots.
		return windows.NewCallback(func(totalLo, totalHi, transferredLo, transferredHi, _, _, _, _, _, _, _, _ uintptr, data *copyProgressFixture) uintptr {
			return data.observe(int64(totalHi)<<32|int64(totalLo), int64(transferredHi)<<32|int64(transferredLo))
		})
	}
	return windows.NewCallback(func(total, transferred int64, _, _, _, _, _, _ uintptr, data *copyProgressFixture) uintptr {
		return data.observe(total, transferred)
	})
})

func (p *copyProgressFixture) attach(flag *int32, watcherDone <-chan struct{}) (uintptr, unsafe.Pointer) {
	p.flag = flag
	p.watcherDone = watcherDone
	return copyProgressRoutine(), unsafe.Pointer(p)
}

func (p *copyProgressFixture) observe(total, transferred int64) uintptr {
	const progressContinue = 0
	if p.transferred != 0 || transferred <= 0 || transferred >= total {
		return progressContinue
	}
	p.total, p.transferred = total, transferred
	if p.err = p.ctx.Err(); p.err != nil {
		return progressContinue
	}
	p.cancel()
	// Hold the native copy at partial progress until the watcher sets
	// pbCancel. PROGRESS_CONTINUE lets CopyFileExW observe the flag itself.
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-p.watcherDone:
		if atomic.LoadInt32(p.flag) != 1 {
			p.err = errors.New("copy watcher returned without setting pbCancel")
		}
	case <-timer.C:
		p.err = errors.New("copy watcher did not set pbCancel within 5 seconds")
	}
	return progressContinue
}

func TestCreateRejectsUndersizedLinuxBaseDisk(t *testing.T) {
	dir := t.TempDir()
	kernel, rootfs := filepath.Join(dir, "vmlinux"), filepath.Join(dir, "rootfs.ext4")
	for _, path := range []string{kernel, rootfs} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Truncate(rootfs, 2<<30); err != nil {
		t.Fatal(err)
	}
	r := &Runtime{opts: Options{Kernel: kernel, Rootfs: rootfs, VMsDir: dir}}
	// A path under a regular file cannot be prepared. The disk-size error
	// must occur before the runtime reaches that step.
	spec := vmruntime.Spec{
		SandboxID: "sandbox",
		GuestOS:   ownsandv1.GuestOs_GUEST_OS_LINUX,
		Dir:       filepath.Join(kernel, "vm"),
		Resources: &ownsandv1.Resources{DiskGib: 1},
	}
	vm, err := r.Create(t.Context(), spec)
	if vm != nil || !errors.Is(err, vmruntime.ErrUnsupported) ||
		!strings.Contains(err.Error(), "needs at least 2 GiB of disk (requested 1 GiB)") {
		t.Fatalf("Create = %v, %v, want the disk-size error", vm, err)
	}
}

func TestDiskPreparationCancellation(t *testing.T) {
	const (
		linux    = ownsandv1.GuestOs_GUEST_OS_LINUX
		windows  = ownsandv1.GuestOs_GUEST_OS_WINDOWS
		copyDisk = "copy disk"
		growDisk = "grow disk"
		copyMeta = "copy metadata"
	)
	for _, tc := range []struct {
		name      string
		guestOS   ownsandv1.GuestOs
		wantCalls []string
		abortCopy bool
	}{
		{"Linux copy interrupted", linux, []string{copyDisk}, true},
		{"Linux copy finishes as cancellation arrives", linux, []string{copyDisk}, false},
		{"Linux growth in progress", linux, []string{copyDisk, growDisk}, false},
		{"Windows copy interrupted", windows, []string{copyDisk}, true},
		{"Windows copy finishes as cancellation arrives", windows, []string{copyDisk}, false},
		{"Windows growth in progress", windows, []string{copyDisk, copyMeta, growDisk}, false},
		{"Windows metadata copy interrupted", windows, []string{copyDisk, copyMeta}, true},
		{"Windows metadata finishes as cancellation arrives", windows, []string{copyDisk, copyMeta}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			r := &Runtime{
				opts:   Options{Kernel: filepath.Join(dir, "kernel"), VMsDir: dir},
				logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
				vms:    map[string]*vm{},
			}
			if err := os.WriteFile(r.opts.Kernel, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			failOpenSystem(t, &hcsError{op: "HcsOpenComputeSystem", hr: hrSystemNotFound})
			spec := vmruntime.Spec{
				SandboxID: "sandbox",
				GuestOS:   tc.guestOS,
				Dir:       r.vmDir("sandbox"),
				Resources: &ownsandv1.Resources{DiskGib: 2},
			}
			if tc.guestOS == windows {
				spec.Snapshot = &vmruntime.SnapshotSource{Dir: filepath.Join(dir, "snapshot")}
				writeWindowsGeneration(t, spec.Snapshot.Dir, snapWindowsDisk, "snapshot", 0, 0)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cancelAt := tc.wantCalls[len(tc.wantCalls)-1]
			var calls []string
			record := func(call string) {
				calls = append(calls, call)
				if call == cancelAt {
					cancel()
				}
			}
			grow := func(_ string, size int64) (int64, error) {
				record(growDisk)
				return size, nil
			}
			ops := vmCreateOps{
				copyFile: func(copyCtx context.Context, src, dst string) error {
					if copyCtx != ctx || copyCtx.Err() != nil {
						t.Fatalf("copy %q requires the live create context", src)
					}
					if err := os.WriteFile(dst, []byte("copy"), 0o600); err != nil {
						t.Fatal(err)
					}
					if filepath.Base(dst) == images.MetadataFile {
						record(copyMeta)
					} else {
						record(copyDisk)
					}
					if tc.abortCopy && copyCtx.Err() != nil {
						if err := os.Remove(dst); err != nil {
							t.Fatal(err)
						}
						return copyCtx.Err()
					}
					return nil
				},
				makeFixedVHD: func(path string, size int64, _ bool) (int64, error) {
					return grow(path, size)
				},
				growVHDX: grow,
				grantVM: func(string, string) error {
					t.Fatal("attempted to grant disk access after cancellation")
					return nil
				},
				createSystem: func(string, any) (*system, error) {
					t.Fatal("attempted to create a compute system after cancellation")
					return nil, nil
				},
			}
			if v, err := r.createVM(ctx, spec, ops); v != nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("create returned VM %v, error %v, want context.Canceled", v, err)
			}
			if !slices.Equal(calls, tc.wantCalls) {
				t.Errorf("disk operations %v, want %v", calls, tc.wantCalls)
			}
			if len(r.vms) != 0 {
				t.Error("canceled create left a VM registered")
			}
			if _, err := os.Stat(spec.Dir); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("VM directory remains after canceled create: %v", err)
			}
		})
	}
}

// recordOpenSystem makes openSystemFunc return err and record whether the
// marker file of the VM directory still exists when the system is opened.
func recordOpenSystem(t *testing.T, marker string, err error) *[]bool {
	t.Helper()
	var markerPresent []bool
	orig := openSystemFunc
	openSystemFunc = func(string) (*system, error) {
		_, serr := os.Stat(marker)
		markerPresent = append(markerPresent, serr == nil)
		return nil, err
	}
	t.Cleanup(func() { openSystemFunc = orig })
	return &markerPresent
}

func TestResetVMDirStopsLeftoverSystemBeforeRemovingDir(t *testing.T) {
	for _, tc := range []struct {
		name    string
		openErr error
	}{
		{"system absent", &hcsError{op: "HcsOpenComputeSystem", hr: hrSystemNotFound}},
		{"unknown stop outcome", &hcsError{op: "HcsOpenComputeSystem", hr: 0x80070005}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &Runtime{
				opts:   Options{VMsDir: t.TempDir()},
				logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
				vms:    map[string]*vm{},
			}
			dir := r.vmDir("sb")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(dir, "disk.vhd")
			if err := os.WriteFile(marker, []byte("disk"), 0o600); err != nil {
				t.Fatal(err)
			}
			seen := recordOpenSystem(t, marker, tc.openErr)
			if err := r.resetVMDir("sb", dir); err != nil {
				t.Fatalf("resetVMDir = %v, want the create to go on", err)
			}
			if len(*seen) != 1 || !(*seen)[0] {
				t.Errorf("the leftover system must be opened once before the directory is removed, got %v", *seen)
			}
			if _, serr := os.Stat(marker); !os.IsNotExist(serr) {
				t.Errorf("the old VM directory content was not removed: %v", serr)
			}
			if st, serr := os.Stat(dir); serr != nil || !st.IsDir() {
				t.Errorf("the VM directory was not recreated: %v", serr)
			}
		})
	}
}

// stubInventory supplies the HCS load and owner inventory results for tests
// without a Hyper-V host.
func stubInventory(t *testing.T, loadErr error, systems []systemProperties, enumErr error) {
	t.Helper()
	origLoad, origEnum := hcsLoadFunc, enumerateSystemsFunc
	hcsLoadFunc = func() error { return loadErr }
	enumerateSystemsFunc = func(owner string) ([]systemProperties, error) {
		if owner != Owner {
			t.Errorf("enumerated owner %q, want %q", owner, Owner)
		}
		return systems, enumErr
	}
	t.Cleanup(func() { hcsLoadFunc, enumerateSystemsFunc = origLoad, origEnum })
}

// newRunningTestRuntime returns a runtime that tracks one running VM
// ("tracked") and one VM whose compute system already exited ("gone").
func newRunningTestRuntime() *Runtime {
	running := &vm{id: "tracked", exited: make(chan struct{})}
	gone := &vm{id: "gone", exited: make(chan struct{})}
	gone.markExited()
	return &Runtime{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		vms:    map[string]*vm{"tracked": running, "gone": gone},
	}
}

func TestRunningEnumerationFailureReturnsError(t *testing.T) {
	cause := &hcsError{op: "HcsEnumerateComputeSystems", hr: 0x80070005}
	for _, tc := range []struct {
		name    string
		tracked bool
		wantIDs []string
	}{
		{"no tracked VM", false, []string{}},
		{"one tracked VM", true, []string{"tracked"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubInventory(t, nil, nil, cause)
			r := newRunningTestRuntime()
			if !tc.tracked {
				r.vms = map[string]*vm{}
			}
			ids, err := r.Running(t.Context())
			if err == nil || !errors.Is(err, cause) {
				t.Fatalf("Running error = %v, want it to wrap %v", err, cause)
			}
			if len(ids) != len(tc.wantIDs) || (len(ids) == 1 && ids[0] != tc.wantIDs[0]) {
				t.Fatalf("Running IDs = %v, want %v", ids, tc.wantIDs)
			}
		})
	}
}

func TestRunningListsSurvivorsOfThisOwner(t *testing.T) {
	stubInventory(t, nil, []systemProperties{
		{ID: systemIDPrefix + "survivor", Owner: Owner},
		{ID: systemIDPrefix + "tracked", Owner: Owner},  // already tracked: listed once
		{ID: systemIDPrefix + "survivor", Owner: Owner}, // duplicate entry
		{ID: "other-" + "system", Owner: Owner},         // no sandbox prefix
	}, nil)
	ids, err := newRunningTestRuntime().Running(t.Context())
	if err != nil {
		t.Fatalf("Running = %v", err)
	}
	got := map[string]int{}
	for _, id := range ids {
		got[id]++
	}
	if len(got) != 2 || got["tracked"] != 1 || got["survivor"] != 1 {
		t.Fatalf("Running IDs = %v, want tracked and survivor once each", ids)
	}
}

func TestRunningEmptyEnumeration(t *testing.T) {
	stubInventory(t, nil, []systemProperties{}, nil)
	r := &Runtime{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), vms: map[string]*vm{}}
	ids, err := r.Running(t.Context())
	if err != nil || len(ids) != 0 {
		t.Fatalf("Running = %v, %v, want no IDs and no error", ids, err)
	}
	passed, detail := hyperVCheck(func() bool { return true })
	if !passed || !strings.Contains(detail, "0 compute systems") {
		t.Errorf("hyperVCheck = %t, %q, want success with zero compute systems", passed, detail)
	}
}

func TestMissingInventory(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
	}{
		{name: "missing"},
		{name: "null", doc: "null"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			list, err := decodeSystemList(tc.doc)
			if !errors.Is(err, errMissingSystemList) {
				t.Fatalf("decodeSystemList(%q) error = %v, want %v", tc.doc, err, errMissingSystemList)
			}
			stubInventory(t, nil, list, err)
			r := newRunningTestRuntime()
			ids, err := r.Running(t.Context())
			if !slices.Equal(ids, []string{"tracked"}) || !errors.Is(err, errMissingSystemList) {
				t.Errorf("Running = %v, %v, want [tracked] and an error wrapping %v", ids, err, errMissingSystemList)
			}
			clear(r.vms)
			ids, err = r.Running(t.Context())
			if len(ids) != 0 || !errors.Is(err, errMissingSystemList) {
				t.Errorf("Running = %v, %v, want no IDs and an error wrapping %v", ids, err, errMissingSystemList)
			}
			passed, detail := hyperVCheck(func() bool { return true })
			if passed || !strings.Contains(detail, errMissingSystemList.Error()) {
				t.Errorf("hyperVCheck = %t, %q, want failure with %v", passed, detail, errMissingSystemList)
			}
		})
	}
}

// TestRunningLoadFailureKeepsTrackedVMs checks that a host without the HCS
// DLL is not an inventory error: no VM can run there, so the tracked VMs are
// the complete list.
func TestRunningLoadFailureKeepsTrackedVMs(t *testing.T) {
	stubInventory(t, errors.New("computecore.dll is missing"), nil, errors.New("must not be called"))
	ids, err := newRunningTestRuntime().Running(t.Context())
	if err != nil {
		t.Fatalf("Running = %v, want no error", err)
	}
	if len(ids) != 1 || ids[0] != "tracked" {
		t.Fatalf("Running IDs = %v, want [tracked]", ids)
	}
}
