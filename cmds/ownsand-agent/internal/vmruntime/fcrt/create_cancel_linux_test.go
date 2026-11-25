// SPDX-License-Identifier: FSL-1.1-ALv2

package fcrt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// These tests start the test binary as a fake firecracker that serves a
// real API socket, so Runtime.Create runs its real code path: the process
// launch, the snapshot load, the stop of the first process, the cold-boot
// fallback and the failed-create cleanup. They do not run a guest or touch
// /dev/kvm, so they prove the control flow of Create and not that a real
// Firecracker restores or cold boots a snapshot.

const (
	// createAPIHelperEnv makes the test binary act as the fake firecracker
	// API server (see init). It is separate from helperEnv, which only blocks.
	createAPIHelperEnv = "FCRT_TEST_CREATE_API_HELPER"
	// createAPILogEnv names the file where the fake appends one line per
	// event; createAPILoadEnv sets how the fake answers PUT /snapshot/load.
	createAPILogEnv  = "FCRT_TEST_CREATE_API_LOG"
	createAPILoadEnv = "FCRT_TEST_CREATE_API_LOAD"
)

// Answers of the fake to PUT /snapshot/load.
const (
	loadBlock         = "block"           // never answers, until the process is killed
	loadFail          = "fail"            // HTTP 400, as a state file that Firecracker cannot read
	loadResumeAndDrop = "resume-and-drop" // the guest writes to disk before the response is lost
	capturedDisk      = "captured snapshot"
)

// The helper runs from init, which precedes TestMain, so TestMain stays
// unchanged. Launch 1 is the memory-state restore attempt; later launches
// are cold boots, which the fake ends at PUT /vsock with an error, so that no
// test waits for a guest helper.
func init() {
	if os.Getenv(createAPIHelperEnv) != "1" {
		return
	}
	logPath, loadMode := os.Getenv(createAPILogEnv), os.Getenv(createAPILoadEnv)
	var sock string
	for i, a := range os.Args {
		if a == "--api-sock" && i+1 < len(os.Args) {
			sock = os.Args[i+1]
		}
	}
	note := func(line string) {
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			os.Exit(3)
		}
		fmt.Fprintln(f, line)
		f.Close()
	}
	prior, _ := os.ReadFile(logPath)
	launch := strings.Count(string(prior), "launch ") + 1
	note(fmt.Sprintf("launch %d pid %d", launch, os.Getpid()))
	if loadMode == loadResumeAndDrop && launch > 1 {
		disk, err := os.ReadFile(diskFile)
		if err != nil {
			os.Exit(4)
		}
		note("fallback disk: " + string(disk))
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		os.Exit(2)
	}
	fault := func(w http.ResponseWriter, msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"fault_message": msg})
	}
	_ = http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			note(fmt.Sprintf("%d %s %s", launch, req.Method, req.URL.Path))
		}
		switch {
		case req.Method == http.MethodPut && req.URL.Path == "/snapshot/load" && loadMode == loadBlock:
			<-req.Context().Done()
		case req.Method == http.MethodPut && req.URL.Path == "/snapshot/load" && loadMode == loadResumeAndDrop:
			var load snapshotLoad
			if err := json.NewDecoder(req.Body).Decode(&load); err != nil || !load.ResumeVM {
				fault(w, "expected a snapshot load with resume")
				return
			}
			if err := os.WriteFile(diskFile, []byte("changed by resumed guest"), 0o600); err != nil {
				fault(w, err.Error())
				return
			}
			note("restore changed the working disk")
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				os.Exit(5)
			}
			conn.Close()
		case req.Method == http.MethodPut && req.URL.Path == "/snapshot/load":
			fault(w, "cannot load the snapshot")
		case req.Method == http.MethodPut && req.URL.Path == "/vsock" && launch > 1:
			fault(w, "fake cold boot stops here")
		case req.Method == http.MethodGet:
			w.Write([]byte("{}"))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	os.Exit(0)
}

type createFixture struct {
	r    *Runtime
	spec vmruntime.Spec
	log  string
	logs *syncBuffer
}

// syncBuffer collects the structured log of the runtime, which writes from
// several goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// coldFallbackLog is the message that the runtime logs when it decides to
// boot the snapshot disk cold after a failed restore.
const coldFallbackLog = "booting the snapshot disk cold"

// requireNoColdFallback fails when the create decided on a cold boot.
func (f *createFixture) requireNoColdFallback(t *testing.T) {
	t.Helper()
	if strings.Contains(f.logs.String(), coldFallbackLog) {
		t.Fatal("the create fell back to a cold boot although its context had ended")
	}
}

// newCreateFixture returns a runtime whose firecracker is the fake API
// server, and a spec that is eligible for a memory-state restore.
func newCreateFixture(t *testing.T, loadMode string) *createFixture {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "firecracker")
	if err := os.Symlink(self, bin); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "events.log")
	// The runtime starts the child with the environment of the test.
	t.Setenv(createAPIHelperEnv, "1")
	t.Setenv(createAPILogEnv, logPath)
	t.Setenv(createAPILoadEnv, loadMode)

	snap := t.TempDir()
	// The snapshot and the spec both request a disk of 0 GiB, so the sizes
	// match (a memory-state restore needs equal sizes) and the cold boot has
	// nothing to grow. A larger sparse disk would only slow the copy.
	if err := os.WriteFile(filepath.Join(snap, snapDisk), []byte(capturedDisk), 0o600); err != nil {
		t.Fatal(err)
	}
	res := &ownsandv1.Resources{VcpuCount: 1, MemoryMib: 128}
	r := bareRuntime(Options{Binary: bin})
	logs := &syncBuffer{}
	r.logger = slog.New(slog.NewJSONHandler(logs, nil))
	// Delete stops a process that a failed assertion left behind.
	t.Cleanup(func() { r.Close() })
	return &createFixture{r: r, log: logPath, logs: logs, spec: vmruntime.Spec{
		SandboxID: "sbx", Dir: filepath.Join(t.TempDir(), "vm"), GuestOS: ownsandv1.GuestOs_GUEST_OS_LINUX,
		NetworkMode: ownsandv1.NetworkMode_NETWORK_MODE_NONE, Resources: res, MemoryRestoreAllowed: true,
		Snapshot: &vmruntime.SnapshotSource{SnapshotID: "snap", Dir: snap, HasMemoryState: true, Resources: res},
	}}
}

func (f *createFixture) events(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(f.log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	text := strings.TrimSpace(string(b))
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// create runs Create in a goroutine and returns the channel of its result.
func (f *createFixture) create(ctx context.Context) <-chan error {
	done := make(chan error, 1)
	go func() {
		defer close(done)
		_, err := f.r.Create(ctx, f.spec)
		done <- err
	}()
	return done
}

func (f *createFixture) createWithSurvivingProcess(t *testing.T) (context.CancelFunc, <-chan error) {
	t.Helper()
	signalGroup, stopTimeout := f.r.signalGroup, f.r.stopTimeout
	f.r.signalGroup = func(int) error { return nil }
	// The fake stays alive. This only bounds how long Create waits for it to stop.
	f.r.stopTimeout = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := f.create(ctx)
	t.Cleanup(func() {
		cancel()
		// Drain any unread result and wait for the Create goroutine to
		// close the channel before restoring the stop settings for Close.
		for range done {
		}
		f.r.signalGroup = signalGroup
		f.r.stopTimeout = stopTimeout
	})
	return cancel, done
}

// waitEvent blocks until the fake logged the event. The deadline only bounds
// a failing test; a passing test waits for the event itself.
func (f *createFixture) waitEvent(t *testing.T, event string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		for _, e := range f.events(t) {
			if e == event {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fake firecracker never logged %q: %q", event, f.events(t))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (f *createFixture) launches(t *testing.T) int {
	n := 0
	for _, e := range f.events(t) {
		if strings.HasPrefix(e, "launch ") {
			n++
		}
	}
	return n
}

// firstPID returns the PID that the first fake process logged.
func (f *createFixture) firstPID(t *testing.T) int {
	t.Helper()
	for _, e := range f.events(t) {
		var n, pid int
		if _, err := fmt.Sscanf(e, "launch %d pid %d", &n, &pid); err == nil && n == 1 {
			return pid
		}
	}
	t.Fatal("the fake firecracker logged no launch")
	return 0
}

func (f *createFixture) registered() int {
	f.r.mu.Lock()
	defer f.r.mu.Unlock()
	return len(f.r.vms)
}

func requireProcessGone(t *testing.T, pid int) {
	t.Helper()
	// The runtime must reap the child before the PID disappears.
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatalf("process %d of the first launch still exists: %v", pid, err)
	}
}

func requireReleased(t *testing.T, f *createFixture) {
	t.Helper()
	if _, err := os.Stat(f.spec.Dir); !os.IsNotExist(err) {
		t.Fatalf("VM directory kept: %v", err)
	}
	if n := f.registered(); n != 0 {
		t.Fatalf("%d failed VMs still registered", n)
	}
}

// A create whose context ends during the snapshot load reports the context
// error, starts no replacement process and releases the VM.
func TestCreateContextEndedDuringRestoreStartsNoReplacement(t *testing.T) {
	cases := []struct {
		name string
		want error
		// ctx returns the create context and the function that ends it.
		ctx func() (context.Context, func())
	}{
		{"canceled", context.Canceled, func() (context.Context, func()) {
			ctx, cancel := context.WithCancel(context.Background())
			return ctx, cancel
		}},
		{"deadline", context.DeadlineExceeded, func() (context.Context, func()) {
			// The test expires the deadline itself, once the load is in
			// flight, so no timer decides the order.
			m := newManualDeadlineContext()
			return m, m.expire
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCreateFixture(t, loadBlock)
			ctx, end := tc.ctx()
			t.Cleanup(end)
			done := f.create(ctx)
			f.waitEvent(t, "1 PUT /snapshot/load")
			end()
			if err := <-done; !errors.Is(err, tc.want) {
				t.Fatalf("create error = %v, want %v", err, tc.want)
			}
			if n := f.launches(t); n != 1 {
				t.Fatalf("%d firecracker launches, want 1", n)
			}
			f.requireNoColdFallback(t)
			requireProcessGone(t, f.firstPID(t))
			requireReleased(t, f)
		})
	}
}

// The context can end while the first process stops, after the restore failed
// with a live context.
func TestCreateContextEndedWhileStoppingFailedRestore(t *testing.T) {
	f := newCreateFixture(t, loadFail)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	kill := f.r.signalGroup
	f.r.signalGroup = func(pid int) error {
		cancel()
		return kill(pid)
	}
	_, err := f.r.Create(ctx, f.spec)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("create error = %v, want a canceled context", err)
	}
	if n := f.launches(t); n != 1 {
		t.Fatalf("%d firecracker launches, want 1", n)
	}
	// The restore failed while the context was live, so the fallback log is
	// expected here; the launch count shows that no cold boot started.
	requireProcessGone(t, f.firstPID(t))
	requireReleased(t, f)
}

// When the first process does not exit, the cleanup keeps the registration
// and the directory so that Close and the next agent start can stop it, and
// Create still reports the context error.
func TestCreateContextEndedKeepsVMWhenProcessSurvives(t *testing.T) {
	f := newCreateFixture(t, loadBlock)
	cancel, done := f.createWithSurvivingProcess(t)
	f.waitEvent(t, "1 PUT /snapshot/load")
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("create error = %v, want a canceled context", err)
	}
	f.requireNoColdFallback(t)
	if n := f.launches(t); n != 1 {
		t.Fatalf("%d firecracker launches, want 1", n)
	}
	if _, err := os.Stat(f.spec.Dir); err != nil {
		t.Fatalf("VM directory not kept: %v", err)
	}
	if n := f.registered(); n != 1 {
		t.Fatalf("%d VMs registered, want the 1 with the surviving process", n)
	}
}

func TestCreateContextEndedReapsSurvivingProcessOnEarlyExit(t *testing.T) {
	cases := []struct {
		name          string
		receiveResult bool
	}{
		{name: "after_result", receiveResult: true},
		{name: "in_flight", receiveResult: false},
	}
	for _, tc := range cases {
		var f *createFixture
		var pid int
		if t.Run(tc.name, func(t *testing.T) {
			f = newCreateFixture(t, loadBlock)
			var done <-chan error
			// Observe the state after the Create cleanup and before Close.
			t.Cleanup(func() {
				select {
				case _, ok := <-done:
					if ok {
						t.Error("create result still pending before runtime close")
					}
				default:
					t.Error("create still running before runtime close")
				}
				if n := f.registered(); n != 1 {
					t.Errorf("%d VMs registered before runtime close, want 1", n)
				}
			})
			cancel, done := f.createWithSurvivingProcess(t)
			f.waitEvent(t, "1 PUT /snapshot/load")
			pid = f.firstPID(t)
			select {
			case err := <-done:
				t.Fatalf("create returned before cancellation: %v", err)
			default:
			}
			if tc.receiveResult {
				cancel()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatalf("create error = %v, want a canceled context", err)
				}
			}
			// Run the same cleanup as a fatal assertion and let the parent
			// check the child after this subtest returns.
			t.SkipNow()
		}) {
			requireProcessGone(t, pid)
			requireReleased(t, f)
		}
	}
}

// A snapshot-load error with a live context still stops the process of the
// attempt and boots the snapshot disk cold with a new process.
func TestCreateLoadErrorWithLiveContextBootsCold(t *testing.T) {
	f := newCreateFixture(t, loadResumeAndDrop)
	_, err := f.r.Create(context.Background(), f.spec)
	if err == nil {
		t.Fatal("create succeeded although the fake cold boot ends in an error")
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("create error = %v, want no context error", err)
	}
	if n := f.launches(t); n != 2 {
		t.Fatalf("%d firecracker launches, want 2 (restore, then cold boot)", n)
	}
	if !strings.Contains(f.logs.String(), coldFallbackLog) {
		t.Fatal("the create did not log the cold-boot fallback")
	}
	events := f.events(t)
	for _, want := range []string{"restore changed the working disk", "fallback disk: " + capturedDisk, "2 PUT /drives/rootfs"} {
		if !slices.Contains(events, want) {
			t.Fatalf("missing event %q: %q", want, events)
		}
	}
	saved, err := os.ReadFile(filepath.Join(f.spec.Snapshot.Dir, snapDisk))
	if err != nil || string(saved) != capturedDisk {
		t.Fatalf("saved snapshot disk = %q, err = %v", saved, err)
	}
	requireProcessGone(t, f.firstPID(t))
	requireReleased(t, f)
}

func TestCreateFallbackCopyFailureReleasesVM(t *testing.T) {
	f := newCreateFixture(t, loadFail)
	kill := f.r.signalGroup
	removeSnapshot := sync.OnceFunc(func() {
		if err := os.Remove(filepath.Join(f.spec.Snapshot.Dir, snapDisk)); err != nil {
			t.Errorf("remove snapshot before fallback: %v", err)
		}
	})
	f.r.signalGroup = func(pid int) error {
		removeSnapshot()
		return kill(pid)
	}
	_, err := f.r.Create(context.Background(), f.spec)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("create error = %v, want missing snapshot disk", err)
	}
	if n := f.launches(t); n != 1 {
		t.Fatalf("%d firecracker launches, want 1", n)
	}
	requireProcessGone(t, f.firstPID(t))
	requireReleased(t, f)
}
