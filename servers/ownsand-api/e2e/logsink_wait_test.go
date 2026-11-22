// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"context"
	"slices"
	"testing"
	"time"
)

func appendLines(logs *logSink, lines ...string) {
	logs.mu.Lock()
	defer logs.mu.Unlock()
	logs.lines = append(logs.lines, lines...)
}

func TestWaitLogMessagesDelayedRecords(t *testing.T) {
	logs := &logSink{}
	appendLines(logs, `api: {"level":"INFO","msg":"billing_sandbox_paused"}`)
	go func() {
		time.Sleep(150 * time.Millisecond)
		appendLines(logs, `api: {"level":"INFO","msg":"billing_sandbox_resumed"}`)
	}()
	start := time.Now()
	missing := logs.waitForLogMessages(context.Background(), "api", 10*time.Second, "billing_sandbox_paused", "billing_sandbox_resumed")
	if len(missing) != 0 {
		t.Fatalf("missing = %q, want none", missing)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("the wait took %s, want it to end soon after the delayed record", elapsed)
	}
}

func TestWaitLogMessagesReportsMissing(t *testing.T) {
	logs := &logSink{}
	appendLines(logs,
		`api: {"level":"INFO","msg":"billing_sandbox_paused"}`,
		// Another process and a non-JSON line must not satisfy the wait.
		`agent0: {"level":"INFO","msg":"billing_sandbox_resumed"}`,
		`api: billing_sandbox_resumed`,
	)
	start := time.Now()
	missing := logs.waitForLogMessages(context.Background(), "api", 200*time.Millisecond, "billing_sandbox_paused", "billing_sandbox_resumed")
	if want := []string{"billing_sandbox_resumed"}; !slices.Equal(missing, want) {
		t.Fatalf("missing = %q, want %q", missing, want)
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("the wait took %s, want it to use its 200ms budget", elapsed)
	}
}

func TestWaitLogMessagesContextCancelEndsWait(t *testing.T) {
	logs := &logSink{}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	missing := logs.waitForLogMessages(ctx, "api", time.Minute, "billing_sandbox_paused")
	if want := []string{"billing_sandbox_paused"}; !slices.Equal(missing, want) {
		t.Fatalf("missing = %q, want %q", missing, want)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the wait took %s after the cancel, want it to end at once", elapsed)
	}
}

func TestWaitLogMessagesNoMessages(t *testing.T) {
	if missing := (&logSink{}).waitForLogMessages(context.Background(), "api", time.Second); len(missing) != 0 {
		t.Fatalf("missing = %q, want none", missing)
	}
}
