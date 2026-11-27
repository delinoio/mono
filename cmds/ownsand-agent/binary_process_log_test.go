// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func waitForProcessSandboxStarted(logs *logBuffer, sandboxID string, pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		stderr := logs.String()
		for line := range strings.Lines(stderr) {
			// The stderr copier can expose a record before its newline arrives.
			if !strings.HasSuffix(line, "\n") {
				break
			}
			var event struct {
				Message   string `json:"msg"`
				SandboxID string `json:"sandbox_id"`
				PID       int    `json:"pid"`
			}
			if json.Unmarshal([]byte(line), &event) == nil &&
				event.Message == "process sandbox started" && event.SandboxID == sandboxID && event.PID == pid {
				return nil
			}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("timed out waiting for process sandbox started for sandbox %q with guest PID %d\nstderr:\n%s", sandboxID, pid, stderr)
		}
		time.Sleep(min(20*time.Millisecond, remaining))
	}
}

func TestWaitForProcessSandboxStarted(t *testing.T) {
	const (
		sandboxID = "sandbox-1"
		pid       = 123
		record    = `{"pid": 123, "msg": "process sandbox started", "sandbox_id": "sandbox-1"}` + "\n"
	)
	for _, tc := range []struct {
		name       string
		firstWrite string
	}{
		{name: "delayed record"},
		{name: "split record", firstWrite: record[:7]},
		{name: "delayed newline", firstWrite: strings.TrimSuffix(record, "\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const delay = 50 * time.Millisecond
				var logs logBuffer
				fmt.Fprint(&logs, tc.firstWrite)
				done := make(chan struct{})
				go func() {
					defer close(done)
					time.Sleep(delay)
					fmt.Fprint(&logs, record[len(tc.firstWrite):])
				}()
				start := time.Now()
				err := waitForProcessSandboxStarted(&logs, sandboxID, pid, time.Second)
				elapsed := time.Since(start)
				<-done
				if err != nil {
					t.Fatal(err)
				}
				if elapsed < delay {
					t.Fatalf("accepted a record before the final write after %s", elapsed)
				}
			})
		})
	}

	t.Run("unrelated records time out", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			const timeout = 45 * time.Millisecond
			const stderr = `non-JSON stderr
{"msg":"process sandbox started","sandbox_id":"sandbox-1","pid":1234}
{"msg":"another event","sandbox_id":"sandbox-1","pid":123}
{"msg":"process sandbox started","sandbox_id":"other","pid":123}
{"msg":"process sandbox started","sandbox_id":"sandbox-1","pid":"123"}
{"msg":"process sandbox started","sandbox_id":"sandbox-1","pid":123.0}
`
			var logs logBuffer
			fmt.Fprint(&logs, stderr)
			start := time.Now()
			err := waitForProcessSandboxStarted(&logs, sandboxID, pid, timeout)
			if err == nil || !strings.Contains(err.Error(), `sandbox "sandbox-1" with guest PID 123`) ||
				!strings.Contains(err.Error(), stderr) {
				t.Fatalf("timeout error = %v, want sandbox ID, PID and stderr", err)
			}
			if elapsed := time.Since(start); elapsed != timeout {
				t.Fatalf("wait lasted %s, want %s", elapsed, timeout)
			}
			fmt.Fprint(&logs, record)
			if err := waitForProcessSandboxStarted(&logs, sandboxID, pid, timeout); err != nil {
				t.Fatal(err)
			}
		})
	})
}
