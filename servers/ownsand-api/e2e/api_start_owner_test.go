// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// These tests need no database and run without OWNSAND_E2E. They drive the
// wait of startAPI (pollReadyWhile with the server_started ownership check)
// against a foreign server that answers 200, and they trigger every change of
// the log or of the exit channel from the ownership check itself, so none of
// them depends on timer granularity.

const ownerAddr = "127.0.0.1:41000"

func startedLine(name, addr string) string {
	return name + `: {"level":"INFO","msg":"server_started","addr":"` + addr + `","stun_addr":"127.0.0.1:41001"}`
}

func foreignHealthServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestHasServerStartedMatchesProcessAndAddress(t *testing.T) {
	logs := &logSink{}
	appendLines(logs,
		startedLine("api", "127.0.0.1:41999"),
		startedLine("api-attempt2", ownerAddr),
		`api: server_started `+ownerAddr,
		`api: {"level":"INFO","msg":"server_started_extra","addr":"`+ownerAddr+`"}`,
		`api: {"level":"ERROR","msg":"server_exited","addr":"`+ownerAddr+`"}`,
	)
	if logs.hasServerStarted("api", ownerAddr) {
		t.Error("api matched: its only server_started record has another address")
	}
	if !logs.hasServerStarted("api-attempt2", ownerAddr) {
		t.Error("api-attempt2 must match its own server_started record")
	}
	if logs.hasServerStarted("api-attempt3", ownerAddr) {
		t.Error("a process name is matched exactly")
	}
}

// A foreign 200 does not count while the process owns nothing, and the wait
// ends with errProcessExited when the process exits.
func TestStartAPIWaitRejectsAForeignAnswerUntilTheProcessExits(t *testing.T) {
	srv := foreignHealthServer(t)
	logs := &logSink{}
	// The foreign server answers on a port of its own; the process claims
	// another address in its record, so the record is not ownership.
	appendLines(logs, startedLine("api", "127.0.0.1:41999"))
	exited := make(chan struct{})
	checks := 0
	owned := func() bool {
		checks++
		if checks == 3 {
			// The process exits after it logged server_exited.
			appendLines(logs, `api: {"level":"ERROR","msg":"server_exited","error":"bind: address already in use","startup_failure_reason":"address_in_use"}`)
			close(exited)
		}
		return logs.hasServerStarted("api", ownerAddr)
	}
	err := pollReadyWhile(srv.URL, 30*time.Second, exited, owned)
	if !errors.Is(err, errProcessExited) {
		t.Fatalf("pollReadyWhile = %v, want errProcessExited", err)
	}
	if checks != 3 {
		t.Fatalf("ownership checks = %d, want 3 (every answer is checked until the exit)", checks)
	}
	// The exit leads to the retry on new ports.
	if !shouldRetryAPIStart(err, 1, logs, "api") {
		t.Error("the exit with address_in_use does not lead to a retry")
	}
}

// The record of another attempt (process name) does not give ownership.
func TestStartAPIWaitIgnoresTheRecordOfAnotherAttempt(t *testing.T) {
	srv := foreignHealthServer(t)
	logs := &logSink{}
	appendLines(logs, startedLine("api", ownerAddr))
	exited := make(chan struct{})
	owned := func() bool {
		defer func() {
			select {
			case <-exited:
			default:
				close(exited)
			}
		}()
		return logs.hasServerStarted("api-attempt2", ownerAddr)
	}
	if err := pollReadyWhile(srv.URL, 30*time.Second, exited, owned); !errors.Is(err, errProcessExited) {
		t.Fatalf("pollReadyWhile = %v, want errProcessExited", err)
	}
}

// The wait returns once the process logs server_started for the address.
func TestStartAPIWaitSucceedsAfterServerStarted(t *testing.T) {
	srv := foreignHealthServer(t)
	logs := &logSink{}
	checks := 0
	owned := func() bool {
		checks++
		if checks == 2 {
			appendLines(logs, startedLine("api", ownerAddr))
		}
		return logs.hasServerStarted("api", ownerAddr)
	}
	if err := pollReadyWhile(srv.URL, 30*time.Second, nil, owned); err != nil {
		t.Fatalf("pollReadyWhile: %v", err)
	}
	if checks != 2 {
		t.Fatalf("ownership checks = %d, want 2 (the first answer came before the record)", checks)
	}
}

// pollHealthyWhile keeps its documented contract: a healthy server with an
// open or nil exit channel returns nil at once, a closed one wins.
func TestPollHealthyWhileAnswerAndExitChannel(t *testing.T) {
	srv := foreignHealthServer(t)
	if err := pollHealthyWhile(srv.URL, 30*time.Second, nil); err != nil {
		t.Fatalf("nil exited: %v", err)
	}
	if err := pollHealthyWhile(srv.URL, 30*time.Second, make(chan struct{})); err != nil {
		t.Fatalf("open exited: %v", err)
	}
	closed := make(chan struct{})
	close(closed)
	if err := pollHealthyWhile(srv.URL, 30*time.Second, closed); !errors.Is(err, errProcessExited) {
		t.Fatalf("closed exited: %v, want errProcessExited", err)
	}
}
