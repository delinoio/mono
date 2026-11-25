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
	"testing"
	"time"

	"github.com/Microsoft/go-winio"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
)

// The console log of a VM never exceeds the cap, whatever the guest writes
// and however often the runtime re-dials the pipe; the copy keeps draining
// the pipe after the cap, so the guest never blocks.
func TestCopyConsoleCapsLogAcrossRedials(t *testing.T) {
	f := startConsoleFixture(t, "console-cap-test")

	// 3 MiB in the first connection, then a second connection after the
	// runtime re-dials. Both must be drained completely: a guest write that
	// the copy does not drain hits its deadline and fails the test.
	first := bytes.Repeat([]byte("a"), 3<<20)
	second := bytes.Repeat([]byte("b"), 1<<20)
	for _, data := range [][]byte{first, second} {
		conn := f.accept()
		f.write(conn, data)
		conn.Close()
	}

	if err := f.stopAndWait(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(f.dir, consoleFile))
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(got)) != vmruntime.ConsoleLogLimit {
		t.Fatalf("console.log = %d bytes, want the cap %d", len(got), vmruntime.ConsoleLogLimit)
	}
	if !bytes.Equal(got, first[:vmruntime.ConsoleLogLimit]) {
		t.Fatal("console.log does not keep the first bytes of the guest output")
	}
}

// A pipe that ends while the VM still runs leaves no closer goroutine behind,
// on the real named pipe that the Hyper-V worker process serves: the runtime
// re-dials after each end, and once the pipe is gone no closer from an earlier
// connection is alive. Stopping the VM then ends copyConsole.
func TestCopyConsoleEndsCloserWhenPipeEnds(t *testing.T) {
	f := startConsoleFixture(t, "console-closer-test")

	// Three connections end from the guest side while the VM runs.
	var want []byte
	for _, msg := range []string{"one;", "two;", "three;"} {
		conn := f.accept()
		f.write(conn, []byte(msg))
		conn.Close()
		want = append(want, msg...)
	}
	// Remove the pipe, so the runtime cannot dial again; its live connection,
	// if any, ends with the listener. The VM has not stopped.
	f.listener.Close()
	waitConsoleClosers(t, 0)
	select {
	case <-f.v.consoleDone:
		t.Fatal("copyConsole returned while the VM still runs")
	default:
	}

	if err := f.stopAndWait(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(f.dir, consoleFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("console.log = %q, want every connection appended: %q", got, want)
	}
}

// newTestBuildVM returns a BuildVM that has no compute system: copyConsole
// uses only its stop channel, its exited channel, its console channel and
// its logger.
func newTestBuildVM() *BuildVM {
	return &BuildVM{
		sys:     newSystem("fake-build", 0),
		logger:  slog.New(slog.NewJSONHandler(io.Discard, nil)),
		stop:    make(chan struct{}),
		console: make(chan struct{}),
	}
}

func TestBuildVMRequiresConfirmedExit(t *testing.T) {
	shortStopTimeout(t)
	b := newTestBuildVM()
	registerFakeCallback(t, b.sys)
	b.sys.terminateFn = func() error { return nil }
	b.sys.verifyExitFn = func(string, time.Time) (bool, error) { return false, errors.New("status unavailable") }
	notifySystem(b.sys, hcsEventServiceDisconnect)
	if err := b.WaitExit(t.Context(), 50*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitExit = %v, want unconfirmed shutdown", err)
	}
	requireOpenSignal(t, b.Exited(), "build VM exit")
	go func() {
		<-b.stop
		close(b.console)
	}()
	b.Close()
	b.Close()
	waitForSignal(t, b.sys.closed, "handle closed")
	waitForSignal(t, b.console, "build console stopped")
	requireOpenSignal(t, b.Exited(), "build VM exit")
	if err := b.WaitExit(t.Context(), time.Second); !errors.Is(err, errSystemHandleClosed) {
		t.Fatalf("WaitExit after Close = %v, want handle closed error", err)
	}
}

func TestBuildVMWaitExitAcceptsConfirmation(t *testing.T) {
	for _, statusCheck := range []bool{false, true} {
		b := newTestBuildVM()
		registerFakeCallback(t, b.sys)
		if statusCheck {
			b.sys.verifyExitFn = func(string, time.Time) (bool, error) { return true, nil }
			notifySystem(b.sys, hcsEventServiceDisconnect)
		} else {
			notifySystem(b.sys, hcsEventSystemExited)
		}
		if err := b.WaitExit(t.Context(), time.Second); err != nil {
			t.Fatal(err)
		}
		waitForSignal(t, b.Exited(), "build VM exit")
	}
}

func waitBuildVMConsole(t *testing.T, b *BuildVM) {
	t.Helper()
	select {
	case <-b.console:
	case <-time.After(30 * time.Second):
		t.Fatal("BuildVM.copyConsole did not return")
	}
}

// A pipe that ends while the build VM still runs leaves no closer goroutine
// behind, on a real named pipe: BuildVM.copyConsole re-dials after each end,
// and once the pipe is gone no closer from an earlier connection is alive.
// Closing stop then ends copyConsole and closes b.console.
func TestBuildVMConsoleEndsCloserWhenPipeEnds(t *testing.T) {
	pipe := `\\.\pipe\ownsand-buildvm-closer-test-com1`
	l, err := winio.ListenPipe(pipe, nil)
	if err != nil {
		t.Fatalf("listen on %s: %v", pipe, err)
	}
	defer l.Close()

	path := filepath.Join(t.TempDir(), consoleFile)
	b := newTestBuildVM()
	go b.copyConsole(pipe, path)

	var want []byte
	for _, msg := range []string{"one;", "two;", "three;"} {
		conn, err := l.Accept()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write([]byte(msg)); err != nil {
			t.Fatalf("guest write: %v", err)
		}
		conn.Close()
		want = append(want, msg...)
	}
	l.Close()
	waitConsoleClosers(t, 0)
	select {
	case <-b.console:
		t.Fatal("copyConsole returned while the build VM still runs")
	default:
	}

	close(b.stop)
	waitBuildVMConsole(t, b)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("console.log = %q, want every connection appended: %q", got, want)
	}
}

// Closing stop, or the exit of the compute system, closes a live console
// connection that blocks, ends copyConsole and then closes b.console.
func TestBuildVMConsoleStopClosesLiveConnection(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(b *BuildVM)
	}{
		{"stop", func(b *BuildVM) { close(b.stop) }},
		{"exited", func(b *BuildVM) { close(b.sys.exited) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pipe := `\\.\pipe\ownsand-buildvm-live-` + tc.name + `-test-com1`
			l, err := winio.ListenPipe(pipe, nil)
			if err != nil {
				t.Fatalf("listen on %s: %v", pipe, err)
			}
			defer l.Close()

			path := filepath.Join(t.TempDir(), consoleFile)
			b := newTestBuildVM()
			go b.copyConsole(pipe, path)

			// The guest side stays open and writes once, so the copy
			// blocks in Read on a live connection.
			conn, err := l.Accept()
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if _, err := conn.Write([]byte("boot;")); err != nil {
				t.Fatalf("guest write: %v", err)
			}
			waitConsoleClosers(t, 1)
			select {
			case <-b.console:
				t.Fatal("copyConsole returned before the build VM stopped")
			case <-time.After(50 * time.Millisecond):
			}

			tc.stop(b)
			waitBuildVMConsole(t, b)
			waitConsoleClosers(t, 0)
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "boot;" {
				t.Fatalf("console.log = %q, want boot;", got)
			}
		})
	}
}

// A console log that cannot open ends copyConsole at once, without a dial,
// and still closes b.console.
func TestBuildVMConsoleLogOpenFailureReturnsWithoutDial(t *testing.T) {
	pipe := `\\.\pipe\ownsand-buildvm-openfail-test-com1`
	l, err := winio.ListenPipe(pipe, nil)
	if err != nil {
		t.Fatalf("listen on %s: %v", pipe, err)
	}
	defer l.Close()
	accepted := make(chan struct{})
	go func() {
		if conn, err := l.Accept(); err == nil {
			conn.Close()
			close(accepted)
		}
	}()

	b := newTestBuildVM()
	// The parent directory does not exist, so OpenFile fails.
	go b.copyConsole(pipe, filepath.Join(t.TempDir(), "missing", consoleFile))
	waitBuildVMConsole(t, b)
	select {
	case <-accepted:
		t.Fatal("copyConsole dialed the pipe after the log open failed")
	case <-time.After(300 * time.Millisecond):
	}
	waitConsoleClosers(t, 0)
}
