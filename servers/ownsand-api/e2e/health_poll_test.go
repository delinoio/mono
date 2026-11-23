// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// These tests need no database and run without OWNSAND_E2E.

func TestPollHealthyBoundsAHungRequest(t *testing.T) {
	arrived := make(chan struct{}, 1)
	cancelled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case arrived <- struct{}{}:
		default:
		}
		<-r.Context().Done()
		close(cancelled)
	}))
	defer srv.Close()

	start := time.Now()
	if err := pollHealthy(srv.URL, 300*time.Millisecond); err == nil {
		t.Fatal("pollHealthy succeeded against a server that never answers")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("pollHealthy took %s, want it bounded by the budget", d)
	}
	select {
	case <-arrived:
	default:
		t.Fatal("the request never reached the server")
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the hung request was not cancelled")
	}
}

func TestPollHealthyRetriesUntilReady(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := pollHealthy(srv.URL, 10*time.Second); err != nil {
		t.Fatalf("pollHealthy: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("calls = %d, want 2", got)
	}
}

// The probe that is running when the budget expires ends with the context
// deadline; the timeout error must still report the last status that a
// probe observed.
func TestPollHealthyReportsLastStatusOnTimeout(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	err := pollHealthy(srv.URL, 500*time.Millisecond)
	if err == nil {
		t.Fatal("pollHealthy succeeded against a server that never became healthy")
	}
	if !strings.Contains(err.Error(), "unexpected status 503") {
		t.Fatalf("pollHealthy error = %q, want it to report the last status (unexpected status 503)", err)
	}
	if calls.Load() < 2 {
		t.Fatalf("calls = %d, want the second probe to be held until the deadline", calls.Load())
	}
}

// A server that answers on the probed port does not count when the probed
// process exited: another process can hold a port that the process could
// not bind.
func TestPollHealthyWhileStopsWhenTheProcessExited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	exited := make(chan struct{})
	close(exited)

	start := time.Now()
	err := pollHealthyWhile(srv.URL, 10*time.Second, exited)
	if !errors.Is(err, errProcessExited) {
		t.Fatalf("pollHealthyWhile = %v, want errProcessExited", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("pollHealthyWhile took %s, want it to stop when the process exited", d)
	}
}

// A 200 from another server on the address does not count when the probed
// process exited, even though that answer arrives at once: the exit channel
// is consulted before the 200 is accepted.
func TestPollHealthyWhileIgnoresA200AfterTheProcessExited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	exited := make(chan struct{})
	close(exited)

	start := time.Now()
	err := pollHealthyWhile(srv.URL, 10*time.Second, exited)
	if !errors.Is(err, errProcessExited) {
		t.Fatalf("pollHealthyWhile = %v, want errProcessExited", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("pollHealthyWhile took %s, want it to stop when the process exited", d)
	}
}

// A 200 counts only after the process shows that it owns the listener, so
// another server that holds the address cannot stand in for it while the
// process is still running.
func TestPollReadyWhileWaitsForOwnership(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var checks atomic.Int32
	owned := func() bool { return checks.Add(1) > 3 }
	if err := pollReadyWhile(srv.URL, 10*time.Second, nil, owned); err != nil {
		t.Fatalf("pollReadyWhile: %v", err)
	}
	if got := checks.Load(); got != 4 {
		t.Fatalf("ownership checks = %d, want 4 (the 200 must not count before the process owns the listener)", got)
	}
}

// A process that never owns the listener fails when it exits, promptly and
// with the process-exit error, although another server answers 200.
func TestPollReadyWhileFailsPromptlyWhenTheProcessExitsUnowned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	exited := make(chan struct{})
	time.AfterFunc(300*time.Millisecond, func() { close(exited) })

	start := time.Now()
	err := pollReadyWhile(srv.URL, 10*time.Second, exited, func() bool { return false })
	if !errors.Is(err, errProcessExited) {
		t.Fatalf("pollReadyWhile = %v, want errProcessExited", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("pollReadyWhile took %s, want it to stop when the process exited", d)
	}
}
