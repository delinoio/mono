// SPDX-License-Identifier: FSL-1.1-ALv2

package relay

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// idleSendEntryWait is how long an idle test waits for the writer to enter
// its blocked fake send. The writer enters it as soon as Run starts, so the
// bound only matters when something is wrong.
const idleSendEntryWait = 2 * time.Second

// idleSendEntryError waits until the fake send closes entered. It returns an
// error that names the failure when Run returns first (done delivers its
// result) or when neither happens within within. The idle tests need the send
// to be blocked before the idle interval starts, so they must not wait for
// it without a bound and must tell these two failures apart.
func idleSendEntryError(entered <-chan struct{}, done <-chan error, within time.Duration) error {
	select {
	case <-entered:
		return nil
	default:
	}
	timer := time.NewTimer(within)
	defer timer.Stop()
	select {
	case <-entered:
		return nil
	case err := <-done:
		return fmt.Errorf("Run exited before the send was entered: %w", err)
	case <-timer.C:
		return fmt.Errorf("the send was not entered within %v", within)
	}
}

// awaitIdleSendEntry fails the test with a specific message unless the fake
// send is entered, see idleSendEntryError.
func awaitIdleSendEntry(t *testing.T, entered <-chan struct{}, done <-chan error) {
	t.Helper()
	if err := idleSendEntryError(entered, done, idleSendEntryWait); err != nil {
		t.Fatal(err)
	}
}

// newIdleTestConn returns a connection on a stepClock, which stands still
// until the test advances it. The idle interval then starts when the test
// advances the clock, after it has blocked the send and opened what it needs,
// and not at the construction of the connection, so a slow test machine
// cannot let the idle check win the race against the setup. The connection
// closes when the test ends, so an early failure leaves no Run behind.
func newIdleTestConn(t *testing.T) (*Conn, *stepClock) {
	t.Helper()
	clk := newStepClock()
	c := newConnWithClock(context.Background(), "device", 1, testLimits, clk)
	t.Cleanup(func() { c.Close(errors.New("test done")) })
	return c, clk
}

func TestIdleSendEntryErrorReportsEntry(t *testing.T) {
	entered := make(chan struct{})
	close(entered)
	// Entry wins over a result that is also ready.
	done := make(chan error, 1)
	done <- ErrIdle
	if err := idleSendEntryError(entered, done, time.Second); err != nil {
		t.Fatalf("entered send: %v", err)
	}
	pending := make(chan struct{})
	go func() {
		time.Sleep(10 * time.Millisecond)
		close(pending)
	}()
	if err := idleSendEntryError(pending, make(chan error), 2*time.Second); err != nil {
		t.Fatalf("send entered later: %v", err)
	}
}

func TestIdleSendEntryErrorReportsRunExitBeforeEntry(t *testing.T) {
	done := make(chan error, 1)
	done <- ErrIdle
	err := idleSendEntryError(make(chan struct{}), done, 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "Run exited before the send was entered") || !errors.Is(err, ErrIdle) {
		t.Fatalf("Run exit before entry: %v, want a Run-exited error that wraps ErrIdle", err)
	}
}

func TestIdleSendEntryErrorReportsMissingEntryAtDeadline(t *testing.T) {
	start := time.Now()
	err := idleSendEntryError(make(chan struct{}), make(chan error), 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "the send was not entered within 20ms") {
		t.Fatalf("missing entry: %v, want a not-entered error", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("missing entry was reported after %v, want about the 20ms deadline", elapsed)
	}
}
