// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// startupFailureReasonAddressInUse is the stable startup_failure_reason that
// ownsand-api logs on its server_exited event when it cannot bind its HTTP or
// STUN address because another socket holds it. The value does not depend on
// the operating system or on the wording of its error text.
const startupFailureReasonAddressInUse = "address_in_use"

// hasStartupFailure reports whether the process name logged a server_exited
// record whose startup_failure_reason is reason. Only the records of that
// process count, so an event of an earlier or later attempt (another process
// name) never matches, and neither does error text without the field.
func (l *logSink) hasStartupFailure(name, reason string) bool {
	prefix := name + ": "
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		text, ok := strings.CutPrefix(line, prefix)
		if !ok {
			continue
		}
		var event struct {
			Msg    string `json:"msg"`
			Reason string `json:"startup_failure_reason"`
		}
		// Lines that are not JSON log records are not events.
		if json.Unmarshal([]byte(text), &event) != nil {
			continue
		}
		if event.Msg == "server_exited" && event.Reason == reason {
			return true
		}
	}
	return false
}

// shouldRetryAPIStart decides whether startAPI starts the control plane again
// on new ports after the failed attempt number attempt (counted from 1) of
// the process name. It retries only when the process exited and logged the
// structured address_in_use reason itself, and an attempt remains. A timeout,
// any other exit and the event of another process name end the start.
func shouldRetryAPIStart(err error, attempt int, logs *logSink, name string) bool {
	return errors.Is(err, errProcessExited) &&
		attempt < apiStartAttempts &&
		logs.hasStartupFailure(name, startupFailureReasonAddressInUse)
}

// hasServerStarted reports whether the process name logged a server_started
// record whose addr is exactly addr. server_started is logged after the
// process bound its HTTP listener, and addr is the address that the listener
// reports, in the same text form as the address that freeTCPPort returns. A
// record of another process name or another address does not match.
func (l *logSink) hasServerStarted(name, addr string) bool {
	prefix := name + ": "
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		text, ok := strings.CutPrefix(line, prefix)
		if !ok {
			continue
		}
		var event struct {
			Msg  string `json:"msg"`
			Addr string `json:"addr"`
		}
		// Lines that are not JSON log records are not events.
		if json.Unmarshal([]byte(text), &event) != nil {
			continue
		}
		if event.Msg == "server_started" && event.Addr == addr {
			return true
		}
	}
	return false
}

// requireAPILogMessages is requireLogMessages for the control plane that runs
// now. startAPI names the process api on the first attempt and api-attempt<N>
// after a retry on new ports, and restartAPI names it api-restarted, so a test
// must not pass a literal process name: it reads the log of s.api.
func (s *stack) requireAPILogMessages(ctx context.Context, t *testing.T, msgs ...string) {
	t.Helper()
	s.logs.requireLogMessages(ctx, t, s.api.name, msgs...)
}

// apiHasLineContaining reports whether the control plane that runs now logged
// a line that contains text.
func (s *stack) apiHasLineContaining(text string) bool {
	return s.logs.hasLineContaining(s.api.name, text)
}
