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
