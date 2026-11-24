// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

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
	s := &system{id: "fake", exited: make(chan struct{})}
	s.terminateFn = func() error { return terminate(s) }
	return s
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
	for _, tc := range []struct {
		name      string
		terminate func(s *system) error
		wantErr   error
		wantTerm  bool
	}{
		{"terminate and exit event", exitsOnTerminate, nil, false},
		{"terminate error but exit event", func(s *system) error { s.markExited(); return termErr }, nil, false},
		{"terminate without exit event", neverExits, errStopUnconfirmed, false},
		{"terminate error without exit event", func(*system) error { return termErr }, errStopUnconfirmed, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &Runtime{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			sys := fakeSystem(tc.terminate)
			err := r.stopSystem("sb", sys)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("stopSystem = %v, want %v", err, tc.wantErr)
			}
			if tc.wantTerm && !errors.Is(err, termErr) {
				t.Errorf("stopSystem = %v, want it to wrap the terminate error", err)
			}
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
	shortStopTimeout(t)
	r, dir := newDeleteTestRuntime(t, "sb")
	v := &vm{id: "sb", dir: dir, exited: make(chan struct{}), stopping: make(chan struct{}),
		sys: fakeSystem(neverExits)}
	r.vms["sb"] = v
	if err := r.Delete(t.Context(), "sb"); !errors.Is(err, errStopUnconfirmed) {
		t.Fatalf("Delete = %v, want errStopUnconfirmed", err)
	}
	if r.vms["sb"] != v {
		t.Error("Delete dropped the registration of a VM whose exit is not confirmed")
	}
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
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, bytes.Repeat([]byte{7}, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst")
	if err := copyFile(context.Background(), src, dst); err != nil {
		t.Fatalf("copy on a live context = %v", err)
	}
	if got, err := os.ReadFile(dst); err != nil || len(got) != 1<<20 {
		t.Fatalf("copy = %d bytes, %v", len(got), err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := copyFile(canceled, src, dst); !errors.Is(err, context.Canceled) {
		t.Fatalf("copy on a canceled context = %v, want context.Canceled", err)
	}
	// A copy that is canceled while it runs. The file is large so that the
	// copy outlasts the cancellation; the test skips when the file system
	// copies it before the cancellation arrives (for example a ReFS clone).
	big := filepath.Join(dir, "big")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(2 << 30); err != nil {
		f.Close()
		t.Skipf("the large source file could not be created: %v", err)
	}
	f.Close()
	bigDst := filepath.Join(dir, "big-dst")
	ctx, cancelMid := context.WithCancel(context.Background())
	time.AfterFunc(time.Millisecond, cancelMid)
	err = copyFile(ctx, big, bigDst)
	if err == nil {
		t.Skip("the copy finished before the cancellation arrived")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled copy = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(bigDst); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial copy remains after a canceled copy: %v", err)
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
		{"Windows growth in progress", windows, []string{copyDisk, growDisk}, false},
		{"Windows metadata copy interrupted", windows, []string{copyDisk, growDisk, copyMeta}, true},
		{"Windows metadata finishes as cancellation arrives", windows, []string{copyDisk, growDisk, copyMeta}, false},
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

// stubInventory makes Running see the given HCS load result and compute
// systems for the test, without a Hyper-V host.
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
	stubInventory(t, nil, nil, nil)
	r := &Runtime{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), vms: map[string]*vm{}}
	ids, err := r.Running(t.Context())
	if err != nil || len(ids) != 0 {
		t.Fatalf("Running = %v, %v, want no IDs and no error", ids, err)
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
