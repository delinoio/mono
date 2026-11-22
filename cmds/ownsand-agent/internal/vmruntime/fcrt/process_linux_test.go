// SPDX-License-Identifier: FSL-1.1-ALv2

package fcrt

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// helperEnv makes the test binary act as a long-running stand-in for
// firecracker: the real-process tests start it through a symbolic link named
// firecracker.
const helperEnv = "FCRT_TEST_FIRECRACKER_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		// A bare `select {}` is killed by Go's deadlock detector when the
		// binary is built with CGO_ENABLED=0 (no runnable goroutines and no
		// pending timers). Waiting on a signal channel keeps the signal
		// handling goroutine alive. Only SIGUSR2 is caught, so SIGTERM and
		// SIGKILL still end the helper as they would end firecracker.
		block := make(chan os.Signal, 1)
		signal.Notify(block, syscall.SIGUSR2)
		<-block
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// bareRuntime returns a runtime without a network manager and with a quiet
// logger, for tests that start fake firecracker processes.
func bareRuntime(opts Options) *Runtime {
	opts.Logger = quiet()
	r, err := newRuntime(opts, func() error { return nil })
	if err != nil {
		panic(err)
	}
	return r
}

// fakeProc is one fixture process. immortal keeps it alive after a kill, as
// a process stuck in uninterruptible sleep does.
type fakeProc struct {
	start    uint64
	args     []string
	alive    bool
	immortal bool
}

type fakeHandle struct {
	host *fakeHost
	pid  int
	proc *fakeProc
}

func (h *fakeHandle) kill() error {
	h.host.mu.Lock()
	defer h.host.mu.Unlock()
	h.host.signals = append(h.host.signals, h.pid)
	if !h.proc.alive {
		return errProcessGone
	}
	if !h.proc.immortal {
		h.proc.alive = false
	}
	return nil
}

func (h *fakeHandle) waitExit(time.Duration) bool {
	h.host.mu.Lock()
	defer h.host.mu.Unlock()
	return !h.proc.alive
}

func (h *fakeHandle) close() {
	h.host.mu.Lock()
	h.host.closed++
	h.host.mu.Unlock()
}

type fakeHost struct {
	mu      sync.Mutex
	boot    string
	procs   map[int]*fakeProc
	signals []int
	closed  int
	// onOpen runs inside open, before the handle is bound; it simulates a
	// PID reuse between the check and the open.
	onOpen func(pid int)
	// openErr and cmdlineErr make open and cmdline fail the way an
	// exhausted descriptor table or an unreadable /proc entry does.
	openErr    error
	cmdlineErr error
}

func newFakeHost() *fakeHost { return &fakeHost{boot: "boot-a", procs: map[int]*fakeProc{}} }

func (h *fakeHost) add(pid int, p *fakeProc) *fakeProc {
	h.mu.Lock()
	h.procs[pid] = p
	h.mu.Unlock()
	return p
}

func (h *fakeHost) live(pid int) (*fakeProc, error) {
	p, ok := h.procs[pid]
	if !ok || !p.alive {
		return nil, errProcessGone
	}
	return p, nil
}

func (h *fakeHost) bootID() (string, error) { return h.boot, nil }

func (h *fakeHost) startTime(pid int) (uint64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p, err := h.live(pid)
	if err != nil {
		return 0, err
	}
	return p.start, nil
}

func (h *fakeHost) cmdline(pid int) ([]string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cmdlineErr != nil {
		return nil, h.cmdlineErr
	}
	p, err := h.live(pid)
	if err != nil {
		return nil, err
	}
	return p.args, nil
}

func (h *fakeHost) listPIDs() ([]int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var pids []int
	for pid, p := range h.procs {
		if p.alive {
			pids = append(pids, pid)
		}
	}
	sort.Ints(pids)
	return pids, nil
}

func (h *fakeHost) open(pid int) (procHandle, error) {
	if h.openErr != nil {
		return nil, h.openErr
	}
	if h.onOpen != nil {
		h.onOpen(pid)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	p, err := h.live(pid)
	if err != nil {
		return nil, err
	}
	return &fakeHandle{host: h, pid: pid, proc: p}, nil
}

func (h *fakeHost) signalled() []int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]int(nil), h.signals...)
}

func fcArgs(id string) []string {
	return []string{"/usr/bin/firecracker", "--api-sock", apiSocket, "--id", idPrefix + id}
}

// fixture is a runtime over a fake process table and a VMs directory.
type fixture struct {
	t    *testing.T
	r    *Runtime
	host *fakeHost
	logs *bytes.Buffer
	vms  string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	logs := &bytes.Buffer{}
	vms := t.TempDir()
	r, err := newRuntime(Options{Binary: "firecracker", VMsDir: vms, Logger: slog.New(slog.NewJSONHandler(logs, nil))}, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	host := newFakeHost()
	r.host = host
	r.stopTimeout = 20 * time.Millisecond
	return &fixture{t: t, r: r, host: host, logs: logs, vms: vms}
}

// orphan registers a firecracker process of an earlier agent process with a
// valid launch record and returns its VM directory.
func (f *fixture) orphan(id string, pid int) (string, *fakeProc) {
	f.t.Helper()
	p := f.host.add(pid, &fakeProc{start: 4242, args: fcArgs(id), alive: true})
	dir := filepath.Join(f.vms, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := writeLaunchRecord(f.host, dir, id, pid); err != nil {
		f.t.Fatal(err)
	}
	return dir, p
}

func (f *fixture) exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func TestParseStartTime(t *testing.T) {
	// The command name contains spaces and parentheses; field 22 is 98765.
	stat := "1234 (fire) (cracker )) S 1 1234 1234 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 98765 1000 100 18446744073709551615"
	got, err := parseStartTime(stat)
	if err != nil || got != 98765 {
		t.Fatalf("start time %d, %v", got, err)
	}
	for _, bad := range []string{"", "1234 firecracker S 1", "1 (x) S 1 2"} {
		if _, err := parseStartTime(bad); err == nil {
			t.Fatalf("parsed %q", bad)
		}
	}
}

func TestLinuxProcsReadsProcTree(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("sys/kernel/random/boot_id", "abc-123\n")
	write("77/stat", "77 (fire cracker) S 1 77 77 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 5551 0 0 0\n")
	write("77/cmdline", "/bin/firecracker\x00--id\x00ownsand-x\x00")
	write("self/stat", "")
	p := linuxProcs{root: root}
	if boot, err := p.bootID(); err != nil || boot != "abc-123" {
		t.Fatalf("boot ID %q %v", boot, err)
	}
	if st, err := p.startTime(77); err != nil || st != 5551 {
		t.Fatalf("start time %d %v", st, err)
	}
	if args, err := p.cmdline(77); err != nil || len(args) != 3 || args[2] != "ownsand-x" {
		t.Fatalf("cmdline %q %v", args, err)
	}
	if _, err := p.startTime(78); !errors.Is(err, errProcessGone) {
		t.Fatalf("missing process: %v", err)
	}
	if pids, err := p.listPIDs(); err != nil || len(pids) != 1 || pids[0] != 77 {
		t.Fatalf("pids %v %v", pids, err)
	}
}

func TestVerifyLaunchMatrix(t *testing.T) {
	const id = "sb1"
	cases := []struct {
		name   string
		mutate func(f *fixture, dir string, p *fakeProc)
		want   launchSkipReason // empty: the process is verified
	}{
		{"valid record", func(*fixture, string, *fakeProc) {}, ""},
		{"record missing", func(f *fixture, dir string, _ *fakeProc) {
			os.Remove(filepath.Join(dir, launchRecordFile))
		}, skipRecordMissing},
		{"directory missing", func(f *fixture, dir string, _ *fakeProc) { os.RemoveAll(dir) }, skipRecordMissing},
		{"record unparsable", func(f *fixture, dir string, _ *fakeProc) {
			os.WriteFile(filepath.Join(dir, launchRecordFile), []byte("{not json"), 0o600)
		}, skipRecordUnreadable},
		{"record of an unknown version", func(f *fixture, dir string, _ *fakeProc) {
			os.WriteFile(filepath.Join(dir, launchRecordFile), []byte(`{"version":99,"sandbox_id":"sb1","pid":10,"boot_id":"boot-a","start_time":4242}`), 0o600)
		}, skipRecordUnreadable},
		{"record names another sandbox", func(f *fixture, dir string, _ *fakeProc) {
			os.WriteFile(filepath.Join(dir, launchRecordFile), []byte(`{"version":1,"sandbox_id":"other","pid":10,"boot_id":"boot-a","start_time":4242}`), 0o600)
		}, skipRecordForeign},
		{"record readable by others", func(f *fixture, dir string, _ *fakeProc) {
			os.Chmod(filepath.Join(dir, launchRecordFile), 0o644)
		}, skipRecordForeign},
		{"record is a link", func(f *fixture, dir string, _ *fakeProc) {
			real := filepath.Join(f.vms, "elsewhere.json")
			raw, _ := os.ReadFile(filepath.Join(dir, launchRecordFile))
			os.WriteFile(real, raw, 0o600)
			os.Remove(filepath.Join(dir, launchRecordFile))
			os.Symlink(real, filepath.Join(dir, launchRecordFile))
		}, skipRecordForeign},
		{"directory open to group", func(f *fixture, dir string, _ *fakeProc) { os.Chmod(dir, 0o770) }, skipRecordForeign},
		{"directory is a link", func(f *fixture, dir string, _ *fakeProc) {
			moved := dir + ".real"
			os.Rename(dir, moved)
			os.Symlink(moved, dir)
		}, skipRecordForeign},
		{"host rebooted", func(f *fixture, _ string, _ *fakeProc) { f.host.boot = "boot-b" }, skipBootChanged},
		{"process ended", func(_ *fixture, _ string, p *fakeProc) { p.alive = false }, skipProcessGone},
		{"PID reused with another start time", func(_ *fixture, _ string, p *fakeProc) { p.start = 9999 }, skipProcessChanged},
		{"PID reused by an unrelated command", func(_ *fixture, _ string, p *fakeProc) { p.args = []string{"/bin/sleep", "60"} }, skipProcessChanged},
		{"process of another sandbox", func(_ *fixture, _ string, p *fakeProc) { p.args = fcArgs("other") }, skipProcessChanged},
		{"PID reused between check and open", func(f *fixture, _ string, _ *fakeProc) {
			f.host.onOpen = func(pid int) {
				f.host.mu.Lock()
				f.host.procs[pid] = &fakeProc{start: 7777, args: fcArgs(id), alive: true}
				f.host.mu.Unlock()
			}
		}, skipProcessChanged},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			dir, p := f.orphan(id, 10)
			tc.mutate(f, dir, p)
			got, reason, _ := verifyLaunch(f.host, dir, id)
			if reason != tc.want {
				t.Fatalf("reason %q, want %q", reason, tc.want)
			}
			if (got != nil) != (tc.want == "") {
				t.Fatalf("verified process %v with reason %q", got, reason)
			}
			if got != nil {
				got.handle.close()
			}
			if tc.want != "" {
				if f.host.closed != 0 && tc.want != skipProcessChanged {
					t.Fatalf("%d handles opened for a skipped process", f.host.closed)
				}
			}
		})
	}
}

// A process that fails the proof is never signalled by Delete, and the skip
// is logged with its reason; the leftover directory is still removed.
func TestDeleteSkipsUnverifiedProcess(t *testing.T) {
	cases := map[string]struct {
		setup  func(f *fixture)
		reason launchSkipReason
	}{
		"pre-upgrade VM without record": {func(f *fixture) {
			f.host.add(10, &fakeProc{start: 1, args: fcArgs("sb1"), alive: true})
			os.MkdirAll(filepath.Join(f.vms, "sb1"), 0o700)
		}, skipRecordMissing},
		"foreign process with the same --id": {func(f *fixture) {
			f.orphan("sb1", 10)
			f.host.procs[10].start = 5 // PID reused
		}, skipProcessChanged},
		"unreadable record": {func(f *fixture) {
			dir, _ := f.orphan("sb1", 10)
			os.WriteFile(filepath.Join(dir, launchRecordFile), []byte("garbage"), 0o600)
		}, skipRecordUnreadable},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			tc.setup(f)
			if err := f.r.Delete(context.Background(), "sb1"); err != nil {
				t.Fatal(err)
			}
			if got := f.host.signalled(); len(got) != 0 {
				t.Fatalf("signalled %v", got)
			}
			if !f.host.procs[10].alive {
				t.Fatal("unverified process was killed")
			}
			if out := f.logs.String(); !strings.Contains(out, `"reason":"`+string(tc.reason)+`"`) || !strings.Contains(out, `"sandbox_id":"sb1"`) {
				t.Fatalf("no diagnostic with reason %q in logs: %s", tc.reason, out)
			}
			if f.exists(filepath.Join(f.vms, "sb1")) {
				t.Fatal("VM directory kept")
			}
		})
	}
}

// A verified orphan is killed; while it survives the bounded wait Delete
// fails and keeps the directory, and a repeated Delete finishes.
func TestDeleteOrphanWaitsForExit(t *testing.T) {
	f := newFixture(t)
	dir, p := f.orphan("sb1", 10)
	p.immortal = true
	err := f.r.Delete(context.Background(), "sb1")
	if err == nil || !strings.Contains(err.Error(), "did not stop") {
		t.Fatalf("Delete of a surviving orphan: %v", err)
	}
	if !f.exists(filepath.Join(dir, launchRecordFile)) {
		t.Fatal("launch record removed while the orphan still runs")
	}
	if got := f.host.signalled(); len(got) != 1 || got[0] != 10 {
		t.Fatalf("signalled %v", got)
	}
	// The process ends (the kernel finished tearing it down).
	f.host.mu.Lock()
	p.alive = false
	f.host.mu.Unlock()
	if err := f.r.Delete(context.Background(), "sb1"); err != nil {
		t.Fatalf("Delete after the exit: %v", err)
	}
	if f.exists(dir) {
		t.Fatal("VM directory kept after the exit")
	}
	if f.host.closed != 1 {
		t.Fatalf("%d handles closed, want the 1 that was opened", f.host.closed)
	}
}

// An operational failure while verifying a process (here: no descriptor for
// the process handle, or an unreadable command line) is not proof that the
// process is not ours: Delete fails and keeps the directory and the launch
// record, so that a retry can still prove ownership. Running reports the
// failure instead of hiding the VM.
func TestVerificationErrorsKeepVMState(t *testing.T) {
	cases := map[string]func(h *fakeHost){
		"process handle cannot be opened": func(h *fakeHost) { h.openErr = syscall.EMFILE },
		"command line cannot be read":     func(h *fakeHost) { h.cmdlineErr = syscall.EACCES },
	}
	for name, inject := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			dir, p := f.orphan("sb1", 10)
			inject(f.host)
			if err := f.r.Delete(context.Background(), "sb1"); err == nil {
				t.Fatal("Delete succeeded although the process could not be verified")
			}
			if !f.exists(filepath.Join(dir, launchRecordFile)) {
				t.Fatal("launch record removed after a verification error")
			}
			if _, err := f.r.Running(context.Background()); err == nil {
				t.Fatal("Running hid a VM that could not be verified")
			}
			// The retry succeeds once the host works again.
			f.host.openErr, f.host.cmdlineErr = nil, nil
			if err := f.r.Delete(context.Background(), "sb1"); err != nil {
				t.Fatalf("retry: %v", err)
			}
			if p.alive {
				t.Fatal("process survived the retry")
			}
			if f.exists(dir) {
				t.Fatal("VM directory kept after the retry")
			}
		})
	}
}

func TestDeleteAbsentVMIsIdempotent(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 2; i++ {
		if err := f.r.Delete(context.Background(), "nothing"); err != nil {
			t.Fatal(err)
		}
	}
	// The same with an empty process table and no VMs directory at all.
	r, err := newRuntime(Options{Binary: "firecracker", Logger: quiet()}, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	r.host = newFakeHost()
	if err := r.Delete(context.Background(), "nothing"); err != nil {
		t.Fatal(err)
	}
}

// trackedVM registers a VM whose firecracker child is a fixture: the exit
// channel stays open until the test closes it.
func (f *fixture) trackedVM(id string, pid int) (*vm, chan struct{}) {
	f.t.Helper()
	dir := filepath.Join(f.vms, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	dirFile, err := os.Open(dir)
	if err != nil {
		f.t.Fatal(err)
	}
	exited := make(chan struct{})
	v := &vm{id: id, dir: dir, dirFile: dirFile, exited: exited, cmd: &exec.Cmd{Process: &os.Process{Pid: pid}}}
	f.r.mu.Lock()
	f.r.vms[id] = v
	f.r.mu.Unlock()
	return v, exited
}

func TestDeleteTrackedWaitsForExit(t *testing.T) {
	f := newFixture(t)
	v, exited := f.trackedVM("sb1", 321)
	var mu sync.Mutex
	var signalled []int
	f.r.signalGroup = func(pid int) error {
		mu.Lock()
		signalled = append(signalled, pid)
		mu.Unlock()
		return nil // the signal is delivered, the process does not end
	}
	err := f.r.Delete(context.Background(), "sb1")
	if err == nil || !strings.Contains(err.Error(), "did not stop") {
		t.Fatalf("Delete of a surviving child: %v", err)
	}
	if _, lerr := f.r.lookup("sb1"); lerr != nil {
		t.Fatal("registration dropped while the process lives")
	}
	if !f.exists(v.dir) {
		t.Fatal("VM directory removed while the process lives")
	}
	if ids, _ := f.r.Running(context.Background()); len(ids) != 1 {
		t.Fatalf("Running %v, want the surviving VM", ids)
	}
	if _, err := v.dirFile.Stat(); err != nil {
		t.Fatalf("directory handle closed while the process lives: %v", err)
	}
	close(exited)
	if err := f.r.Delete(context.Background(), "sb1"); err != nil {
		t.Fatalf("Delete after the exit: %v", err)
	}
	if _, lerr := f.r.lookup("sb1"); lerr == nil {
		t.Fatal("VM still registered")
	}
	if f.exists(v.dir) {
		t.Fatal("VM directory kept")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(signalled) != 1 || signalled[0] != 321 {
		t.Fatalf("signalled %v: an exited child must not be signalled again", signalled)
	}
	if err := f.r.Delete(context.Background(), "sb1"); err != nil {
		t.Fatalf("repeated Delete: %v", err)
	}
}

// A tracked child that already ended is not signalled: its PID may name
// another process once the wait goroutine reaped it.
func TestStopDoesNotSignalExitedChild(t *testing.T) {
	f := newFixture(t)
	v, exited := f.trackedVM("sb1", 321)
	close(exited)
	f.r.signalGroup = func(int) error { t.Fatal("signalled an exited child"); return nil }
	if err := f.r.stop(v); err != nil {
		t.Fatal(err)
	}
}

func TestRunningListsOnlyVerifiedOrphans(t *testing.T) {
	f := newFixture(t)
	f.orphan("verified", 10)
	f.host.add(11, &fakeProc{start: 1, args: fcArgs("norecord"), alive: true})
	os.MkdirAll(filepath.Join(f.vms, "norecord"), 0o700)
	f.orphan("stale", 12)
	f.host.procs[12].alive = false
	ids, err := f.r.Running(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "verified" {
		t.Fatalf("Running %v", ids)
	}
	if f.host.closed != 1 {
		t.Fatalf("%d handles closed, want 1", f.host.closed)
	}
	if out := f.logs.String(); !strings.Contains(out, `"sandbox_id":"norecord"`) {
		t.Fatalf("no diagnostic for the unverified process: %s", out)
	}
}

func TestRunningFailsWhenVMDirectoryCannotBeListed(t *testing.T) {
	f := newFixture(t)
	// A regular file in place of the directory makes ReadDir fail with
	// ENOTDIR, an operational error that is not "does not exist".
	file := filepath.Join(t.TempDir(), "vms")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f.r.opts.VMsDir = file
	if ids, err := f.r.Running(context.Background()); err == nil {
		t.Fatalf("Running returned %v without an error", ids)
	}
}

func TestWriteLaunchRecord(t *testing.T) {
	f := newFixture(t)
	dir, _ := f.orphan("sb1", 10)
	st, err := os.Stat(filepath.Join(dir, launchRecordFile))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("record %v %v", st, err)
	}
	rec, reason, err := readLaunchRecord(dir, "sb1")
	if err != nil || reason != "" {
		t.Fatalf("read: %q %v", reason, err)
	}
	if rec.PID != 10 || rec.BootID != "boot-a" || rec.StartTime != 4242 || rec.SandboxID != "sb1" {
		t.Fatalf("record %+v", rec)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temporary files left: %v", entries)
	}
	// A process that is gone cannot be recorded.
	f.host.procs[10].alive = false
	if err := writeLaunchRecord(f.host, dir, "sb1", 10); err == nil {
		t.Fatal("recorded a missing process")
	}
}

// helperBinary links the test binary as firecracker.
func helperBinary(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "firecracker")
	if err := os.Symlink(self, link); err != nil {
		t.Fatal(err)
	}
	return link
}

func startHelper(t *testing.T, bin, dir, id string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin, "--api-sock", apiSocket, "--id", idPrefix+id)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		// os.Process.Kill returns os.ErrProcessDone once Wait reaped the
		// child, so a PID that the kernel may have reused is never signalled.
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("kill helper %d: %v", cmd.Process.Pid, err)
		}
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("helper %d was not reaped within 10s of the kill", cmd.Process.Pid)
		}
	})
	return cmd
}

func processAlive(pid int) bool {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// A zombie of a child that the test did not reap yet counts as ended.
	return !strings.Contains(string(raw[strings.LastIndexByte(string(raw), ')'):]), " Z ")
}

// Against real processes and the live /proc: a launch record written like
// startProcess writes it proves a stand-in firecracker, Running reports it
// and Delete kills it through its pidfd and waits for the exit, while a
// process that merely carries the same --id convention stays untouched.
func TestRealProcessOrphanCleanup(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	bin := helperBinary(t)
	vms := t.TempDir()
	r, err := newRuntime(Options{Binary: bin, VMsDir: vms, Logger: quiet()}, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"owned", "foreign"} {
		if err := os.MkdirAll(filepath.Join(vms, id), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	owned := startHelper(t, bin, filepath.Join(vms, "owned"), "owned")
	foreign := startHelper(t, bin, filepath.Join(vms, "foreign"), "foreign") // no launch record
	if err := writeLaunchRecord(r.host, filepath.Join(vms, "owned"), "owned", owned.Process.Pid); err != nil {
		t.Skipf("pidfd or /proc unavailable: %v", err)
	}
	if p, reason, err := verifyLaunch(r.host, filepath.Join(vms, "owned"), "owned"); p == nil {
		t.Skipf("process not verifiable here (%q %v)", reason, err)
	} else {
		p.handle.close()
	}
	ids, _ := r.Running(context.Background())
	if len(ids) != 1 || ids[0] != "owned" {
		t.Fatalf("Running %v", ids)
	}
	r.stopTimeout = 5 * time.Second
	if err := r.Delete(context.Background(), "owned"); err != nil {
		t.Fatal(err)
	}
	if processAlive(owned.Process.Pid) {
		t.Fatal("verified process still alive after Delete")
	}
	if _, err := os.Stat(filepath.Join(vms, "owned")); !os.IsNotExist(err) {
		t.Fatalf("VM directory kept: %v", err)
	}
	if err := r.Delete(context.Background(), "foreign"); err != nil {
		t.Fatal(err)
	}
	if !processAlive(foreign.Process.Pid) {
		t.Fatal("process without a launch record was killed")
	}
}

// startProcess writes the launch record of the real child, and stop kills
// the child and waits until it ended.
func TestStartProcessRecordsLaunchAndStopKillsChild(t *testing.T) {
	bin := helperBinary(t)
	vms := t.TempDir()
	r, err := newRuntime(Options{Binary: bin, VMsDir: vms, Logger: quiet()}, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	r.stopTimeout = 5 * time.Second
	dir := filepath.Join(vms, "sb1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	dirFile, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dirFile.Close()
	v := &vm{id: "sb1", dir: dir, dirFile: dirFile, network: ownsandv1.NetworkMode_NETWORK_MODE_NONE}
	t.Setenv(helperEnv, "1")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	// The stand-in serves no API socket, so the start fails after the launch.
	if err := r.startProcess(ctx, v); err == nil {
		t.Fatal("start without an API socket succeeded")
	}
	// startProcess keeps the child in v.cmd even when it fails, so a failed
	// assertion below must still end and reap the child. stop is idempotent:
	// after the explicit stop below, v.cmd is nil and this does nothing.
	t.Cleanup(func() { _ = r.stop(v) })
	rec, reason, err := readLaunchRecord(dir, "sb1")
	if err != nil || reason != "" {
		t.Fatalf("launch record: %q %v", reason, err)
	}
	if rec.PID != v.cmd.Process.Pid {
		t.Fatalf("record PID %d, child %d", rec.PID, v.cmd.Process.Pid)
	}
	if p, reason, err := verifyLaunch(r.host, dir, "sb1"); p == nil {
		t.Fatalf("record of the live child not verified: %q %v", reason, err)
	} else {
		p.handle.close()
	}
	if err := r.stop(v); err != nil {
		t.Fatal(err)
	}
	select {
	case <-v.exited:
	default:
		t.Fatal("stop returned before the child ended")
	}
	if v.cmd != nil {
		t.Fatal("stopped child still referenced")
	}
}
