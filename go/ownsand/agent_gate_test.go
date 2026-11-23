// SPDX-License-Identifier: Apache-2.0

package ownsand

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// agentGateTimeout bounds how long fixture setup waits for a request to reach
// the fake agent gate.
const agentGateTimeout = 10 * time.Second

// awaitAgentGate waits until the fake agent reports the gate signal want on
// hits, which means the agent holds the request of the operation op. It
// reports false after it calls fail when the setup cannot finish:
//   - the operation returned on out before the signal, so the request never
//     reached the gate (for example an early validation or transport error),
//   - the signal names another request, or
//   - no signal and no result arrived within timeout.
//
// The helper consumes the operation result only on the failure path, so a
// caller that sees true can still read out later.
func awaitAgentGate(fail func(format string, args ...any), op, want string, hits <-chan string, out <-chan error, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case got := <-hits:
		if got != want {
			fail("%s: agent gate signal %q, want %q", op, got, want)
			return false
		}
		return true
	case err := <-out:
		fail("%s completed before the agent gate signal %q arrived (result: %v)", op, want, err)
		return false
	case <-timer.C:
		fail("%s: agent gate signal %q did not arrive within %v and the operation did not return", op, want, timeout)
		return false
	}
}

// requireAgentGate is awaitAgentGate for fixture setup: it ends the test when
// the gate signal does not arrive.
func requireAgentGate(t *testing.T, op, want string, hits <-chan string, out <-chan error) {
	t.Helper()
	awaitAgentGate(func(format string, args ...any) {
		t.Helper()
		t.Fatalf(format, args...)
	}, op, want, hits, out, agentGateTimeout)
}

// gateFailures records the failure messages that awaitAgentGate reports.
type gateFailures []string

func (g *gateFailures) fail(format string, args ...any) {
	*g = append(*g, strings.TrimSpace(fmt.Sprintf(format, args...)))
}

func TestAwaitAgentGateAcceptsTheMatchingSignal(t *testing.T) {
	hits := make(chan string, 1)
	out := make(chan error, 1)
	hits <- "read"
	var f gateFailures
	if !awaitAgentGate(f.fail, "read", "read", hits, out, time.Minute) {
		t.Fatalf("matching signal rejected: %v", []string(f))
	}
	if len(f) != 0 {
		t.Fatalf("unexpected failures: %v", []string(f))
	}
	// The result stays readable for the later wait.
	out <- errHandleClosed
	if err := <-out; !errors.Is(err, errHandleClosed) {
		t.Fatalf("result %v", err)
	}
}

func TestAwaitAgentGateReportsEarlyCompletion(t *testing.T) {
	hits := make(chan string, 1)
	out := make(chan error, 1)
	out <- errHandleClosed
	var f gateFailures
	if awaitAgentGate(f.fail, "write", "write", hits, out, time.Minute) {
		t.Fatal("early completion accepted")
	}
	if len(f) != 1 {
		t.Fatalf("failures %v, want one", []string(f))
	}
	for _, part := range []string{"write", `"write"`, "before the agent gate signal", errHandleClosed.Error()} {
		if !strings.Contains(f[0], part) {
			t.Fatalf("failure %q does not contain %q", f[0], part)
		}
	}
}

func TestAwaitAgentGateReportsEarlyCompletionWithoutError(t *testing.T) {
	hits := make(chan string, 1)
	out := make(chan error, 1)
	out <- nil
	var f gateFailures
	if awaitAgentGate(f.fail, "read", "read", hits, out, time.Minute) {
		t.Fatal("early completion accepted")
	}
	if len(f) != 1 || !strings.Contains(f[0], "before the agent gate signal") {
		t.Fatalf("failures %v", []string(f))
	}
}

func TestAwaitAgentGateReportsAWrongSignal(t *testing.T) {
	hits := make(chan string, 1)
	out := make(chan error, 1)
	hits <- "write"
	var f gateFailures
	if awaitAgentGate(f.fail, "read", "read", hits, out, time.Minute) {
		t.Fatal("wrong signal accepted")
	}
	if len(f) != 1 || !strings.Contains(f[0], `"write"`) || !strings.Contains(f[0], `"read"`) {
		t.Fatalf("failures %v", []string(f))
	}
}

func TestAwaitAgentGateTimesOut(t *testing.T) {
	hits := make(chan string, 1)
	out := make(chan error, 1)
	var f gateFailures
	// A zero timeout makes the timer fire at once; no signal and no result
	// are pending, so the timeout path is the only ready case.
	if awaitAgentGate(f.fail, "read", "read", hits, out, 0) {
		t.Fatal("timeout accepted")
	}
	if len(f) != 1 {
		t.Fatalf("failures %v, want one", []string(f))
	}
	for _, part := range []string{"read", `"read"`, "did not arrive"} {
		if !strings.Contains(f[0], part) {
			t.Fatalf("failure %q does not contain %q", f[0], part)
		}
	}
}
