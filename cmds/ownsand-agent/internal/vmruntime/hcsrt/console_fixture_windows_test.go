// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
)

// consoleFixture serves the console pipe of one VM on a real named pipe, as
// the Hyper-V worker process does, and runs Runtime.copyConsole against it.
// Every accept and every write has a bound and names its attempt number. When
// the test ends, or fails, the fixture closes the listener and every accepted
// connection and stops and waits for the copy worker, so a failure leaves
// nothing behind.
type consoleFixture struct {
	t        *testing.T
	dir      string
	listener net.Listener
	v        *vm

	stopOnce sync.Once

	mu       sync.Mutex
	conns    []net.Conn
	attempts int
}

func startConsoleFixture(t *testing.T, sandboxID string) *consoleFixture {
	t.Helper()
	pipe := consolePipe(sandboxID)
	l, err := winio.ListenPipe(pipe, nil)
	if err != nil {
		t.Fatalf("listen on %s: %v", pipe, err)
	}
	// TempDir registers its removal first, so the cleanup below, which ends
	// the worker that holds the log file open, runs before the removal.
	dir := t.TempDir()
	f := &consoleFixture{
		t:        t,
		dir:      dir,
		listener: l,
		v: &vm{
			id:          sandboxID,
			dir:         dir,
			stopping:    make(chan struct{}),
			exited:      make(chan struct{}),
			consoleDone: make(chan struct{}),
		},
	}
	r := &Runtime{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
	go r.copyConsole(f.v)
	t.Cleanup(f.cleanup)
	return f
}

// accept waits for the next connection of the runtime. It counts the
// attempts, so a failure names the attempt that did not arrive, and it fails
// the test when none arrives within consoleFixtureBound.
func (f *consoleFixture) accept() net.Conn {
	f.t.Helper()
	f.mu.Lock()
	f.attempts++
	attempt := f.attempts
	f.mu.Unlock()
	conn, err := acceptWithin(f.listener, attempt, consoleFixtureBound)
	if err != nil {
		f.t.Fatal(err)
	}
	f.mu.Lock()
	f.conns = append(f.conns, conn)
	f.mu.Unlock()
	return conn
}

// write sends data to the runtime on the connection that the latest accept
// returned, within consoleFixtureBound.
func (f *consoleFixture) write(conn net.Conn, data []byte) {
	f.t.Helper()
	f.mu.Lock()
	attempt := f.attempts
	f.mu.Unlock()
	if err := writeWithin(conn, attempt, data, consoleFixtureBound); err != nil {
		f.t.Fatal(err)
	}
}

// stopAndWait stops the VM and waits for copyConsole to end.
func (f *consoleFixture) stopAndWait() error {
	f.stopOnce.Do(func() { close(f.v.stopping) })
	select {
	case <-f.v.consoleDone:
		return nil
	case <-time.After(consoleFixtureBound):
		return fmt.Errorf("copyConsole did not end after stopping within %s", consoleFixtureBound)
	}
}

func (f *consoleFixture) cleanup() {
	f.listener.Close()
	f.mu.Lock()
	conns := f.conns
	f.mu.Unlock()
	for _, conn := range conns {
		conn.Close()
	}
	if err := f.stopAndWait(); err != nil {
		f.t.Error(err)
	}
}
