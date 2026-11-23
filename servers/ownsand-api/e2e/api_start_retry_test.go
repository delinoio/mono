// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"errors"
	"fmt"
	"testing"
)

// These tests need no database and run without OWNSAND_E2E.

const (
	exitedInUse = `{"level":"ERROR","msg":"server_exited","error":"listen tcp 127.0.0.1:1: bind: address already in use","startup_failure_reason":"address_in_use"}`
	// The Windows error text has no "address already in use" wording: the
	// decision must come from the reason field alone.
	exitedInUseWindows = `{"level":"ERROR","msg":"server_exited","error":"listen tcp 127.0.0.1:1: bind: Only one usage of each socket address (protocol/network address/port) is normally permitted.","startup_failure_reason":"address_in_use"}`
	exitedOther        = `{"level":"ERROR","msg":"server_exited","error":"config: DATABASE_URL is required"}`
)

var errExitedBeforeHealthy = fmt.Errorf("%w before it became healthy", errProcessExited)

func TestShouldRetryAPIStart(t *testing.T) {
	timeout := fmt.Errorf("not healthy within 1m0s: %w", errors.New("unexpected status 503"))
	cases := []struct {
		name    string
		err     error
		attempt int
		// process is the process name of the attempt; lines are complete sink
		// lines, each prefixed with the name of the process that wrote it.
		process string
		lines   []string
		want    bool
	}{
		{"first collision", errExitedBeforeHealthy, 1, "api", []string{"api: " + exitedInUse}, true},
		{"second collision", errExitedBeforeHealthy, 2, "api-attempt2", []string{"api: " + exitedInUse, "api-attempt2: " + exitedInUse}, true},
		{"third collision ends the start", errExitedBeforeHealthy, 3, "api-attempt3", []string{"api-attempt3: " + exitedInUse}, false},
		{"Windows error text", errExitedBeforeHealthy, 1, "api", []string{"api: " + exitedInUseWindows}, true},
		{"healthy start", nil, 1, "api", []string{"api: " + exitedInUse}, false},
		{"timeout", timeout, 1, "api", []string{"api: " + exitedInUse}, false},
		{"unrelated exit", errExitedBeforeHealthy, 1, "api", []string{"api: " + exitedOther}, false},
		{"no record", errExitedBeforeHealthy, 1, "api", nil, false},
		{"event of another process name", errExitedBeforeHealthy, 2, "api-attempt2", []string{"api: " + exitedInUse, "api-attempt3: " + exitedInUse, "agent0: " + exitedInUse}, false},
		{"error text without the field", errExitedBeforeHealthy, 1, "api", []string{`api: {"level":"ERROR","msg":"server_exited","error":"bind: address already in use"}`}, false},
		{"plain text line", errExitedBeforeHealthy, 1, "api", []string{"api: server_exited startup_failure_reason=address_in_use"}, false},
		{"field on another event", errExitedBeforeHealthy, 1, "api", []string{`api: {"level":"INFO","msg":"server_started","startup_failure_reason":"address_in_use"}`}, false},
	}
	for _, tc := range cases {
		logs := &logSink{}
		appendLines(logs, tc.lines...)
		if got := shouldRetryAPIStart(tc.err, tc.attempt, logs, tc.process); got != tc.want {
			t.Errorf("%s: shouldRetryAPIStart = %v, want %v", tc.name, got, tc.want)
		}
	}
}
