// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt

import (
	"bytes"
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
	const sandboxID = "console-cap-test"
	pipe := consolePipe(sandboxID)
	l, err := winio.ListenPipe(pipe, nil)
	if err != nil {
		t.Fatalf("listen on %s: %v", pipe, err)
	}
	defer l.Close()

	dir := t.TempDir()
	v := &vm{
		id:          sandboxID,
		dir:         dir,
		stopping:    make(chan struct{}),
		exited:      make(chan struct{}),
		consoleDone: make(chan struct{}),
	}
	r := &Runtime{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
	go r.copyConsole(v)

	// 3 MiB in the first connection, then a second connection after the
	// runtime re-dials. Both must be drained completely.
	first := bytes.Repeat([]byte("a"), 3<<20)
	second := bytes.Repeat([]byte("b"), 1<<20)
	for _, data := range [][]byte{first, second} {
		conn, err := l.Accept()
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := conn.Write(data)
			conn.Close()
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("guest write: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("the guest side blocked: the console copy stopped draining the pipe")
		}
	}

	close(v.stopping)
	select {
	case <-v.consoleDone:
	case <-time.After(30 * time.Second):
		t.Fatal("copyConsole did not end after stopping")
	}
	got, err := os.ReadFile(filepath.Join(dir, consoleFile))
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
	const sandboxID = "console-closer-test"
	pipe := consolePipe(sandboxID)
	l, err := winio.ListenPipe(pipe, nil)
	if err != nil {
		t.Fatalf("listen on %s: %v", pipe, err)
	}
	defer l.Close()

	dir := t.TempDir()
	v := &vm{
		id:          sandboxID,
		dir:         dir,
		stopping:    make(chan struct{}),
		exited:      make(chan struct{}),
		consoleDone: make(chan struct{}),
	}
	r := &Runtime{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
	go r.copyConsole(v)

	// Three connections end from the guest side while the VM runs.
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
	// Remove the pipe, so the runtime cannot dial again; its live connection,
	// if any, ends with the listener. The VM has not stopped.
	l.Close()
	waitConsoleClosers(t, 0)
	select {
	case <-v.consoleDone:
		t.Fatal("copyConsole returned while the VM still runs")
	default:
	}

	close(v.stopping)
	select {
	case <-v.consoleDone:
	case <-time.After(30 * time.Second):
		t.Fatal("copyConsole did not end after stopping")
	}
	got, err := os.ReadFile(filepath.Join(dir, consoleFile))
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
		sys:     &system{exited: make(chan struct{})},
		logger:  slog.New(slog.NewJSONHandler(io.Discard, nil)),
		stop:    make(chan struct{}),
		console: make(chan struct{}),
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
