// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"context"
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
	t.Parallel()
	arrived := make(chan struct{}, 1)
	cancelled := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case arrived <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	result := make(chan error, 1)
	go func() {
		result <- pollHealthy(srv.URL, 300*time.Millisecond)
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("pollHealthy succeeded against a server that never answers")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pollHealthy did not return within five seconds")
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
	t.Parallel()
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
	t.Parallel()
	var calls atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	result := make(chan error, 1)
	go func() {
		result <- pollHealthy(srv.URL, 500*time.Millisecond)
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("pollHealthy succeeded against a server that never became healthy")
		}
		if !strings.Contains(err.Error(), "unexpected status 503") {
			t.Fatalf("pollHealthy error = %q, want it to report the last status (unexpected status 503)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pollHealthy did not return within five seconds")
	}
	if calls.Load() < 2 {
		t.Fatalf("calls = %d, want the second probe to be held until the deadline", calls.Load())
	}
}

func TestPollReadyWhileSkipsTheProbeWhenTheProcessAlreadyExited(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	exited := make(chan struct{})
	close(exited)

	if err := pollReadyWhile(srv.URL, 10*time.Second, exited, nil); !errors.Is(err, errProcessExited) {
		t.Fatalf("pollReadyWhile = %v, want errProcessExited", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("calls = %d, want 0", got)
	}
}

func TestPollReadyWhileCancelsAHungRequestOnProcessExit(t *testing.T) {
	t.Parallel()
	arrived := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	exited := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- pollReadyWhile(srv.URL, 10*time.Second, exited, func() bool { return false })
	}()

	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the request never reached the server")
	}
	close(exited)

	timeout := time.After(5 * time.Second)
	select {
	case err := <-result:
		if !errors.Is(err, errProcessExited) {
			t.Fatalf("pollReadyWhile = %v, want errProcessExited", err)
		}
	case <-timeout:
		t.Fatal("pollReadyWhile did not stop after the process exited")
	}
	select {
	case <-canceled:
	case <-timeout:
		t.Fatal("the server did not observe request cancellation")
	}
}

func TestPollReadyWhileRequiresOwnershipBeforeDeadline(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	const lateBudget = 400 * time.Millisecond
	for _, tc := range []struct {
		name   string
		budget time.Duration
		delay  time.Duration
		want   error
	}{
		{name: "timely", budget: 10 * time.Second},
		{name: "late", budget: lateBudget, delay: lateBudget, want: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exited := make(chan struct{})
			checked := false
			err := pollReadyWhile(srv.URL, tc.budget, exited, func() bool {
				checked = true
				time.Sleep(tc.delay)
				return true
			})
			if !checked {
				t.Fatal("the ownership check did not run")
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("pollReadyWhile = %v, want %v", err, tc.want)
			}
			if tc.want != nil && !strings.HasPrefix(err.Error(), "not healthy within 400ms: ") {
				t.Fatalf("pollReadyWhile = %v, want the startup timeout diagnostic", err)
			}
		})
	}
}

func TestPollReadyWhileRejectsA200WhenTheProcessExitsDuringTheOwnershipCheck(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	exited := make(chan struct{})
	owned := func() bool {
		close(exited)
		return true
	}
	if err := pollReadyWhile(srv.URL, 10*time.Second, exited, owned); !errors.Is(err, errProcessExited) {
		t.Fatalf("pollReadyWhile = %v, want errProcessExited", err)
	}
}

// A 200 counts only after the process shows that it owns the listener, so
// another server that holds the address cannot stand in for it while the
// process is still running.
func TestPollReadyWhileWaitsForOwnership(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
