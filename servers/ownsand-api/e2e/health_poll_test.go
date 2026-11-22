// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"errors"
	"net/http"
	"net/http/httptest"
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

func TestPollHealthyReportsLastStatusOnTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	if err := pollHealthy(srv.URL, 300*time.Millisecond); err == nil {
		t.Fatal("pollHealthy succeeded against a 503 server")
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
