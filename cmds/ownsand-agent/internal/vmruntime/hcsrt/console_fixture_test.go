// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// consoleFixtureBound is the failure bound of one accept or one guest write
// in the console fixtures. A passing test never waits for it: it only turns a
// hang, for example a runtime that never dials, into a named failure. It is
// generous, so it does not depend on the timer granularity of the host or on
// the load of CI.
const consoleFixtureBound = 30 * time.Second

// consoleFixtureReclaimBound limits the wait for Accept to return after
// listener closure, including the probe that verifies closure.
const consoleFixtureReclaimBound = 10 * time.Second

// errConsoleFixtureTimeout marks an accept or a write that exceeded its bound.
var errConsoleFixtureTimeout = errors.New("console fixture timeout")

// acceptWithin accepts one connection on l and fails after d. The error names
// the attempt number. A listener has no accept deadline, so Accept runs in a
// goroutine; when d passes, acceptWithin closes l, which ends that Accept, and
// closes a connection that arrived at the same time, so nothing outlives the
// failure. It returns errConsoleFixtureTimeout for a timeout.
func acceptWithin(l net.Listener, attempt int, d time.Duration) (net.Conn, error) {
	type result struct {
		conn net.Conn
		err  error
	}
	// The channel holds the result, so the goroutine ends even when nobody
	// receives it.
	results := make(chan result, 1)
	go func() {
		conn, err := l.Accept()
		results <- result{conn, err}
	}()

	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case r := <-results:
		if r.err != nil {
			return nil, fmt.Errorf("accept attempt %d failed: %w", attempt, r.err)
		}
		return r.conn, nil
	case <-timer.C:
	}

	l.Close()
	reclaim := time.NewTimer(consoleFixtureReclaimBound)
	defer reclaim.Stop()
	select {
	case r := <-results:
		if r.conn != nil {
			r.conn.Close()
		}
	case <-reclaim.C:
	}
	return nil, fmt.Errorf("accept attempt %d timed out after %s: %w", attempt, d, errConsoleFixtureTimeout)
}

// writeWithin writes data to conn and fails after d. The error names the
// attempt number, and conn is closed when the write times out. It returns
// errConsoleFixtureTimeout for a timeout.
func writeWithin(conn net.Conn, attempt int, data []byte, d time.Duration) error {
	if err := conn.SetWriteDeadline(time.Now().Add(d)); err != nil {
		conn.Close()
		return fmt.Errorf("guest write attempt %d: set the write deadline: %w", attempt, err)
	}
	_, err := conn.Write(data)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, os.ErrDeadlineExceeded):
		conn.Close()
		return fmt.Errorf("guest write attempt %d timed out after %s, so the console copy may have stopped draining the pipe: %w", attempt, d, errConsoleFixtureTimeout)
	default:
		return fmt.Errorf("guest write attempt %d failed: %w", attempt, err)
	}
}

// An accept that nobody answers fails with the attempt number, closes the
// listener and ends the pending Accept.
func TestAcceptWithinTimeoutNamesAttemptAndClosesListener(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	_, err = acceptWithin(l, 2, 20*time.Millisecond)
	if !errors.Is(err, errConsoleFixtureTimeout) {
		t.Fatalf("acceptWithin did not return the timeout error: %v", err)
	}
	if !strings.Contains(err.Error(), "accept attempt 2 timed out") {
		t.Fatalf("the error does not name the attempt: %q", err.Error())
	}
	timer := time.NewTimer(consoleFixtureReclaimBound)
	defer timer.Stop()
	probeResult := make(chan error, 1)
	go func() {
		conn, err := l.Accept()
		if conn != nil {
			conn.Close()
		}
		probeResult <- err
	}()

	select {
	case err := <-probeResult:
		if err == nil {
			t.Fatal("listener closure check accepted a connection")
		}
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("listener closure check returned %v, want net.ErrClosed", err)
		}
	case <-timer.C:
		t.Fatalf("listener closure check timed out after %s", consoleFixtureReclaimBound)
	}
}

// A connection that arrives before the bound is returned as it is.
func TestAcceptWithinReturnsConnection(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	client, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	conn, err := acceptWithin(l, 1, consoleFixtureBound)
	if err != nil {
		t.Fatalf("acceptWithin failed: %v", err)
	}
	conn.Close()
}

// A write to a peer that does not read fails at the deadline, names the
// attempt and closes the connection. net.Pipe has no buffer, so a write
// blocks until the peer reads.
func TestWriteWithinTimeoutNamesAttemptAndClosesConn(t *testing.T) {
	guest, runtimeSide := net.Pipe()
	defer guest.Close()
	defer runtimeSide.Close()

	if err := runtimeSide.SetReadDeadline(time.Now().Add(consoleFixtureReclaimBound)); err != nil {
		t.Fatalf("set peer read deadline: %v", err)
	}

	err := writeWithin(guest, 3, []byte("data"), 20*time.Millisecond)
	if !errors.Is(err, errConsoleFixtureTimeout) {
		t.Fatalf("writeWithin did not return the timeout error: %v", err)
	}
	if !strings.Contains(err.Error(), "guest write attempt 3 timed out") {
		t.Fatalf("the error does not name the attempt: %q", err.Error())
	}
	var buf [1]byte
	if n, err := runtimeSide.Read(buf[:]); n != 0 || err != io.EOF {
		t.Fatalf("peer read after write timeout = (%d, %v), want (0, EOF)", n, err)
	}
}

// A write that the peer reads succeeds.
func TestWriteWithinSucceedsWhenPeerReads(t *testing.T) {
	guest, runtimeSide := net.Pipe()
	defer guest.Close()
	defer runtimeSide.Close()
	got := make(chan int, 1)
	go func() {
		buf := make([]byte, 16)
		n, _ := runtimeSide.Read(buf)
		got <- n
	}()

	if err := writeWithin(guest, 1, []byte("data"), consoleFixtureBound); err != nil {
		t.Fatalf("writeWithin failed: %v", err)
	}
	if n := <-got; n != 4 {
		t.Fatalf("the peer read %d bytes, want 4", n)
	}
}
