// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt

import (
	"bytes"
	"errors"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// consoleCloserCount is the number of live closer goroutines of console
// connections in this process.
func consoleCloserCount() int {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return strings.Count(string(buf[:n]), "hcsrt.closeConsoleOnStop(")
}

// waitConsoleClosers polls until exactly want closers are alive.
func waitConsoleClosers(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := consoleCloserCount()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d console closer goroutines, want %d", got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// fakeConsoleConn is a pipe connection whose Read either returns its data at
// once or, when block is set, waits until Close.
type fakeConsoleConn struct {
	net.Conn // nil: only Read and Close are used.
	data     []byte
	block    bool

	mu     sync.Mutex
	closed chan struct{}
	closes int
}

func newFakeConsoleConn(data string, block bool) *fakeConsoleConn {
	return &fakeConsoleConn{data: []byte(data), block: block, closed: make(chan struct{})}
}

func (c *fakeConsoleConn) Read(p []byte) (int, error) {
	if len(c.data) > 0 {
		n := copy(p, c.data)
		c.data = c.data[n:]
		return n, nil
	}
	if c.block {
		<-c.closed
		return 0, net.ErrClosed
	}
	return 0, io.EOF
}

func (c *fakeConsoleConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closes == 0 {
		close(c.closed)
	}
	c.closes++
	return nil
}

func (c *fakeConsoleConn) closeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closes
}

// runConsoleLoop runs copyConsoleLoop in a goroutine and returns a channel
// that closes when it returns.
func runConsoleLoop(dial consoleDialer, out io.Writer, stopping, exited chan struct{}) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		copyConsoleLoop(dial, out, stopping, exited)
	}()
	return done
}

func waitConsoleLoop(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("copyConsoleLoop did not return")
	}
}

// A copy that returns while the VM still runs leaves no closer behind: no
// closer from an earlier copy is alive at the next dial, and every
// connection is closed.
func TestCopyConsoleLoopEndsCloserWithCopy(t *testing.T) {
	stopping, exited := make(chan struct{}), make(chan struct{})
	conns := []*fakeConsoleConn{newFakeConsoleConn("one;", false), newFakeConsoleConn("two;", false), newFakeConsoleConn("three;", false)}
	var out bytes.Buffer
	dials := 0
	dial := func() (net.Conn, error) {
		if got := consoleCloserCount(); got != 0 {
			t.Errorf("dial %d: %d closers alive from earlier copies, want 0", dials+1, got)
		}
		dials++
		if dials > len(conns) {
			close(stopping)
			return nil, errors.New("pipe is gone")
		}
		return conns[dials-1], nil
	}
	waitConsoleLoop(t, runConsoleLoop(dial, &out, stopping, exited))

	if dials != len(conns)+1 {
		t.Errorf("dials = %d, want %d", dials, len(conns)+1)
	}
	for i, c := range conns {
		if c.closeCount() == 0 {
			t.Errorf("connection %d was not closed", i+1)
		}
	}
	if got := out.String(); got != "one;two;three;" {
		t.Errorf("output = %q, want every successful copy appended", got)
	}
	waitConsoleClosers(t, 0)
}

// Stopping the VM during a copy that blocks closes that connection, the copy
// returns and so does the loop. One copy has one closer, and that closer
// closes the live connection.
func TestCopyConsoleLoopStopClosesLiveConnection(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(stopping, exited chan struct{})
	}{
		{"stopping", func(stopping, _ chan struct{}) { close(stopping) }},
		{"exited", func(_, exited chan struct{}) { close(exited) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stopping, exited := make(chan struct{}), make(chan struct{})
			conn := newFakeConsoleConn("boot;", true)
			dialed := false
			dial := func() (net.Conn, error) {
				if dialed {
					return nil, errors.New("unexpected second dial")
				}
				dialed = true
				return conn, nil
			}
			var out bytes.Buffer
			done := runConsoleLoop(dial, &out, stopping, exited)

			waitConsoleClosers(t, 1)
			select {
			case <-done:
				t.Fatal("copyConsoleLoop returned before the VM stopped")
			case <-time.After(50 * time.Millisecond):
			}
			tc.stop(stopping, exited)
			waitConsoleLoop(t, done)

			if conn.closeCount() == 0 {
				t.Error("the live connection was not closed")
			}
			if out.String() != "boot;" {
				t.Errorf("output = %q, want boot;", out.String())
			}
			waitConsoleClosers(t, 0)
		})
	}
}

// A failed dial between two successful copies starts no closer.
func TestCopyConsoleLoopFailedDialStartsNoCloser(t *testing.T) {
	stopping, exited := make(chan struct{}), make(chan struct{})
	first, second := newFakeConsoleConn("a;", false), newFakeConsoleConn("b;", false)
	var out bytes.Buffer
	dials := 0
	dial := func() (net.Conn, error) {
		if got := consoleCloserCount(); got != 0 {
			t.Errorf("dial %d: %d closers alive, want 0", dials+1, got)
		}
		dials++
		switch dials {
		case 1:
			return first, nil
		case 2:
			return nil, errors.New("pipe busy")
		case 3:
			return second, nil
		}
		close(stopping)
		return nil, errors.New("pipe is gone")
	}
	waitConsoleLoop(t, runConsoleLoop(dial, &out, stopping, exited))

	if first.closeCount() == 0 || second.closeCount() == 0 {
		t.Errorf("closes = %d and %d, want both connections closed", first.closeCount(), second.closeCount())
	}
	if got := out.String(); got != "a;b;" {
		t.Errorf("output = %q, want a;b;", got)
	}
	waitConsoleClosers(t, 0)
}
