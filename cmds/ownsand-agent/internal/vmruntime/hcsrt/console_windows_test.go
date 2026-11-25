// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

const buildVMConsoleProbeBound = 300 * time.Millisecond

type buildVMConsoleFixture struct {
	listener net.Listener
	build    *BuildVM
	stop     func()
	exit     func()
}

func listenBuildVMConsole(t *testing.T, name string) (string, net.Listener) {
	t.Helper()
	pipe := fmt.Sprintf(`\\.\pipe\ownsand-buildvm-%s-%d-com1`, name, os.Getpid())
	listener, err := winio.ListenPipe(pipe, &winio.PipeConfig{OutputBufferSize: 4 << 10})
	if err != nil {
		t.Fatalf("listen on %s: %v", pipe, err)
	}
	return pipe, listener
}

func startBuildVMConsoleFixture(t *testing.T, name, path string) *buildVMConsoleFixture {
	t.Helper()
	pipe, listener := listenBuildVMConsole(t, name)
	b := newTestBuildVM()
	f := &buildVMConsoleFixture{
		listener: listener,
		build:    b,
		stop:     sync.OnceFunc(func() { close(b.stop) }),
		exit:     sync.OnceFunc(func() { close(b.sys.exited) }),
	}
	t.Cleanup(func() {
		listener.Close()
		f.stop()
		waitBuildVMConsole(t, b)
	})
	go b.copyConsole(pipe, path)
	return f
}

// newTestBuildVM creates a BuildVM with a fake HCS handle.
func newTestBuildVM() *BuildVM {
	return &BuildVM{
		sys:     newSystem("fake-build", 0),
		logger:  slog.New(slog.NewJSONHandler(io.Discard, nil)),
		stop:    make(chan struct{}),
		console: make(chan struct{}),
	}
}

func acceptBuildVMConsole(t *testing.T, listener net.Listener, attempt int) net.Conn {
	t.Helper()
	conn, err := acceptWithin(listener, attempt, consoleFixtureBound)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
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
	case <-time.After(consoleFixtureBound):
		t.Fatal("BuildVM.copyConsole did not return")
	}
}

// A pipe that ends while the build VM still runs leaves no closer goroutine
// behind, on a real named pipe: BuildVM.copyConsole re-dials after each end,
// and once the pipe is gone no closer from an earlier connection is alive.
// Closing stop then ends copyConsole and closes b.console.
func TestBuildVMConsoleEndsCloserWhenPipeEnds(t *testing.T) {
	path := filepath.Join(t.TempDir(), consoleFile)
	f := startBuildVMConsoleFixture(t, "closer", path)

	var want []byte
	for i, msg := range []string{"one;", "two;", "three;"} {
		attempt := i + 1
		conn := acceptBuildVMConsole(t, f.listener, attempt)
		if err := writeWithin(conn, attempt, []byte(msg), consoleFixtureBound); err != nil {
			t.Fatal(err)
		}
		conn.Close()
		want = append(want, msg...)
	}
	f.listener.Close()
	waitConsoleClosers(t, 0)
	select {
	case <-f.build.console:
		t.Fatal("copyConsole returned while the build VM still runs")
	default:
	}

	f.stop()
	waitBuildVMConsole(t, f.build)
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
		stop func(f *buildVMConsoleFixture)
	}{
		{"stop", func(f *buildVMConsoleFixture) { f.stop() }},
		{"exited", func(f *buildVMConsoleFixture) { f.exit() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), consoleFile)
			f := startBuildVMConsoleFixture(t, "live-"+tc.name, path)

			// The guest side stays open and writes once, so the copy
			// blocks in Read on a live connection.
			conn := acceptBuildVMConsole(t, f.listener, 1)
			if err := writeWithin(conn, 1, []byte("boot;"), consoleFixtureBound); err != nil {
				t.Fatal(err)
			}
			waitConsoleClosers(t, 1)
			select {
			case <-f.build.console:
				t.Fatal("copyConsole returned before the build VM stopped")
			case <-time.After(50 * time.Millisecond):
			}

			tc.stop(f)
			waitBuildVMConsole(t, f.build)
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
	// The parent directory does not exist, so OpenFile fails.
	path := filepath.Join(t.TempDir(), "missing", consoleFile)
	f := startBuildVMConsoleFixture(t, "openfail", path)
	expectBuildVMConsoleAcceptTimeout(t, f.listener, 1)
	waitBuildVMConsole(t, f.build)
	waitConsoleClosers(t, 0)
}

func TestBuildVMConsoleMissingRedialTimesOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), consoleFile)
	f := startBuildVMConsoleFixture(t, "missing-redial", path)
	conn := acceptBuildVMConsole(t, f.listener, 1)
	// Stop the copy while the first connection is live to suppress the redial.
	f.stop()
	waitBuildVMConsole(t, f.build)
	conn.Close()

	expectBuildVMConsoleAcceptTimeout(t, f.listener, 2)
	waitConsoleClosers(t, 0)
}

type buildVMConsoleAcceptProbe struct {
	net.Listener
	done chan struct{}
}

func (p buildVMConsoleAcceptProbe) Accept() (net.Conn, error) {
	defer close(p.done)
	return p.Listener.Accept()
}

func expectBuildVMConsoleAcceptTimeout(t *testing.T, listener net.Listener, attempt int) {
	t.Helper()
	probe := buildVMConsoleAcceptProbe{Listener: listener, done: make(chan struct{})}
	conn, err := acceptWithin(probe, attempt, buildVMConsoleProbeBound)
	if conn != nil {
		conn.Close()
		t.Fatalf("accept attempt %d unexpectedly connected", attempt)
	}
	want := fmt.Sprintf("accept attempt %d timed out", attempt)
	if !errors.Is(err, errConsoleFixtureTimeout) || !strings.Contains(err.Error(), want) {
		t.Fatalf("accept error = %v, want %q", err, want)
	}
	select {
	case <-probe.done:
	default:
		t.Fatalf("accept attempt %d did not exit after closing the listener", attempt)
	}
	checkBuildVMConsoleListenerClosed(t, listener)
}

func checkBuildVMConsoleListenerClosed(t *testing.T, listener net.Listener) {
	t.Helper()
	conn, err := acceptWithin(listener, 1, buildVMConsoleProbeBound)
	if conn != nil {
		conn.Close()
	}
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("accept after listener cleanup = %v, want %v", err, net.ErrClosed)
	}
}

func TestBuildVMConsoleStalledWriteTimesOut(t *testing.T) {
	pipe, listener := listenBuildVMConsole(t, "stalled-write")
	t.Cleanup(func() { listener.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), consoleFixtureBound)
	defer cancel()
	var client net.Conn
	var dialErr error
	dialed := make(chan struct{})
	t.Cleanup(func() {
		<-dialed
		if client != nil {
			client.Close()
		}
	})
	go func() {
		defer close(dialed)
		client, dialErr = winio.DialPipeContext(ctx, pipe)
	}()
	conn := acceptBuildVMConsole(t, listener, 1)
	<-dialed
	if dialErr != nil {
		t.Fatal(dialErr)
	}

	// The client never reads, so this write cannot fit in the pipe buffer.
	err := writeWithin(conn, 1, bytes.Repeat([]byte("a"), 1<<20), buildVMConsoleProbeBound)
	if !errors.Is(err, errConsoleFixtureTimeout) || !strings.Contains(err.Error(), "guest write attempt 1 timed out") {
		t.Fatalf("guest write error = %v, want timeout on attempt 1", err)
	}
	checkBuildVMConsoleConnClosed(t, conn)
}

func TestBuildVMConsoleCleanupClosesLiveConnection(t *testing.T) {
	var f *buildVMConsoleFixture
	var conn net.Conn
	if !t.Run("early-return", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), consoleFile)
		f = startBuildVMConsoleFixture(t, "cleanup", path)
		conn = acceptBuildVMConsole(t, f.listener, 1)
		waitConsoleClosers(t, 1)
	}) {
		return
	}

	checkBuildVMConsoleListenerClosed(t, f.listener)
	checkBuildVMConsoleConnClosed(t, conn)
	select {
	case <-f.build.console:
	default:
		t.Fatal("fixture cleanup returned before copyConsole ended")
	}
	waitConsoleClosers(t, 0)
}

func checkBuildVMConsoleConnClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	err := writeWithin(conn, 1, []byte("x"), buildVMConsoleProbeBound)
	if !errors.Is(err, os.ErrClosed) {
		t.Fatalf("guest write after connection cleanup = %v, want %v", err, os.ErrClosed)
	}
}
