// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"context"
	"testing"
	"time"
)

// These tests need no database and run without OWNSAND_E2E.

const (
	pausedLine  = `: {"level":"INFO","msg":"billing_sandbox_paused"}`
	resumedLine = `: {"level":"INFO","msg":"billing_sandbox_resumed"}`
)

// After a retry the control plane is api-attempt2: the billing records of the
// live process are found, and a record under the literal first-attempt name
// api (the dead first attempt) is not a substitute.
func TestRequireAPILogMessagesReadsTheLiveControlPlane(t *testing.T) {
	logs := &logSink{}
	appendLines(logs,
		"api: "+`{"level":"ERROR","msg":"server_exited","startup_failure_reason":"address_in_use"}`,
		"api-attempt2"+pausedLine,
		"api-attempt2"+resumedLine,
	)
	s := &stack{logs: logs, api: &proc{name: "api-attempt2"}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Nothing fails the test (a failure here would call t.Fatalf).
	s.requireAPILogMessages(ctx, t, "billing_sandbox_paused", "billing_sandbox_resumed")
	if missing := logs.waitForLogMessages(ctx, "api", 50*time.Millisecond, "billing_sandbox_paused"); len(missing) != 1 {
		t.Errorf("the literal name api matched the records of api-attempt2: missing = %q", missing)
	}
	if !s.apiHasLineContaining("billing_sandbox_paused") {
		t.Error("apiHasLineContaining did not find the record of api-attempt2")
	}
}

// Without a retry the process is api and the behavior is the one of the
// literal name.
func TestRequireAPILogMessagesWithoutRetry(t *testing.T) {
	logs := &logSink{}
	appendLines(logs, "api"+pausedLine, "api"+resumedLine, "api-attempt2"+pausedLine)
	s := &stack{logs: logs, api: &proc{name: "api"}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s.requireAPILogMessages(ctx, t, "billing_sandbox_paused", "billing_sandbox_resumed")

	// A process whose records are absent is reported missing, not matched
	// through another process name.
	s.api = &proc{name: "api-attempt3"}
	if missing := logs.waitForLogMessages(ctx, s.api.name, 50*time.Millisecond, "billing_sandbox_resumed"); len(missing) != 1 {
		t.Errorf("api-attempt3 matched the records of another process: missing = %q", missing)
	}
	if s.apiHasLineContaining("billing_sandbox_paused") {
		t.Error("apiHasLineContaining matched a line of another process name")
	}
}
