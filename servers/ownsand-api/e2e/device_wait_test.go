// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/protobuf/proto"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// observations is a controllable view of the control plane for
// awaitDeviceReconciled.
type observations struct {
	mu         sync.Mutex
	online     []string
	reconciled map[string]bool
	listErr    error
}

func (o *observations) list(context.Context) ([]string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.online...), o.listErr
}

func (o *observations) isReconciled(id string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.reconciled[id]
}

func (o *observations) set(f func(o *observations)) {
	o.mu.Lock()
	defer o.mu.Unlock()
	f(o)
}

func waitFor(t *testing.T, timeout time.Duration, o *observations, skip map[string]bool) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return awaitDeviceReconciled(ctx, time.Millisecond, skip, o.list, o.isReconciled, "api")
}

func TestWaitDeviceOnlineWithoutReconciliationDoesNotReturn(t *testing.T) {
	o := &observations{online: []string{"dev-1"}}
	id, err := waitFor(t, 100*time.Millisecond, o, nil)
	if err == nil {
		t.Fatalf("returned %q for a device that is only ONLINE", id)
	}
	for _, want := range []string{"dev-1", "ONLINE", "agent_state_report", "process api"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestWaitDeviceNotOnlineMessage(t *testing.T) {
	o := &observations{}
	_, err := waitFor(t, 50*time.Millisecond, o, nil)
	if err == nil {
		t.Fatal("returned without an online device")
	}
	if !strings.Contains(err.Error(), "no new device is ONLINE") || strings.Contains(err.Error(), "agent_state_report") {
		t.Errorf("error %q does not name the missing ONLINE state only", err)
	}
}

func TestWaitDeviceIgnoresReconciliationOfOtherDevice(t *testing.T) {
	o := &observations{online: []string{"dev-1", "dev-2"}, reconciled: map[string]bool{"dev-2": true}}
	// dev-2 is known (an earlier agent), so only dev-1 counts.
	_, err := waitFor(t, 100*time.Millisecond, o, map[string]bool{"dev-2": true})
	if err == nil || !strings.Contains(err.Error(), "dev-1") {
		t.Fatalf("err = %v, want a timeout that names dev-1", err)
	}
}

func TestWaitDeviceReturnsAfterReconciliation(t *testing.T) {
	o := &observations{online: []string{"dev-1"}, reconciled: map[string]bool{}}
	go func() {
		time.Sleep(30 * time.Millisecond)
		o.set(func(o *observations) { o.reconciled["dev-1"] = true })
	}()
	id, err := waitFor(t, 10*time.Second, o, nil)
	if err != nil || id != "dev-1" {
		t.Fatalf("got %q, %v; want dev-1", id, err)
	}
}

func TestWaitDeviceListErrorIsReturned(t *testing.T) {
	boom := errors.New("boom")
	o := &observations{listErr: boom}
	_, err := waitFor(t, 10*time.Second, o, nil)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap %v", err, boom)
	}
}

func TestLogSinkStateReportMatching(t *testing.T) {
	logs := &logSink{}
	add := func(lines ...string) {
		logs.mu.Lock()
		logs.lines = append(logs.lines, lines...)
		logs.mu.Unlock()
	}
	add(
		`api: {"level":"INFO","msg":"agent_state_report","device_id":"dev-other","sandboxes":0}`,
		`api: {"level":"INFO","msg":"agent_state_report_device_removed","device_id":"dev-1"}`,
		`api: not json agent_state_report dev-1`,
		`api-restarted: {"level":"INFO","msg":"agent_state_report","device_id":"dev-1"}`,
		`agent0: {"level":"INFO","msg":"agent_state_report","device_id":"dev-1"}`,
	)
	// Only the earlier process (api) and other devices or events so far: the
	// current process api-restarted has a report for dev-1, api has none.
	if logs.hasStateReport("api", "dev-1") {
		t.Error("api must not match: its only dev-1 lines are another event and a non-JSON line")
	}
	if !logs.hasStateReport("api-restarted", "dev-1") {
		t.Error("api-restarted must match its own report")
	}
	if logs.hasStateReport("api-restarted", "dev-other") {
		t.Error("a report of an earlier process must not match the restarted process")
	}
	if !logs.hasStateReport("api", "dev-other") {
		t.Error("api must match its own report for dev-other")
	}
	if logs.hasStateReport("agent", "dev-1") {
		t.Error("a process name is matched exactly, not as a prefix")
	}
}

func TestWaitForAgentConfigurationMatchesProcessAndLimits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logs := &logSink{}
		agent := &proc{name: "agent0", done: make(chan struct{})}
		limits := &ownsandv1.DeviceLimitsInput{VcpuCount: proto.Int32(8), MemoryMib: proto.Int64(16384), DiskGib: proto.Int64(100)}
		const matching = `{"msg":"configuration received","vcpu_limit":8,"memory_mib_limit":16384,"disk_gib_limit":100}`
		appendLines(logs,
			`agent0: {"msg":"configuration received","vcpu_limit":0,"memory_mib_limit":0,"disk_gib_limit":0}`,
			`agent0: {"msg":"configuration received","vcpu_limit":4,"memory_mib_limit":16384,"disk_gib_limit":100}`,
			`agent0: {"msg":"configuration received","vcpu_limit":8,"memory_mib_limit":4096,"disk_gib_limit":100}`,
			`agent0: {"msg":"configuration received","vcpu_limit":8,"memory_mib_limit":16384,"disk_gib_limit":20}`,
			`agent0: {"msg":"another event","vcpu_limit":8,"memory_mib_limit":16384,"disk_gib_limit":100}`,
			"agent0: invalid JSON "+matching,
			"agent01: "+matching,
		)

		result := make(chan error, 1)
		go func() { result <- logs.waitForAgentLimits(t.Context(), agent, "dev-1", limits) }()
		synctest.Wait()
		select {
		case err := <-result:
			t.Fatalf("wait returned before the target process logged the expected limits: %v", err)
		default:
		}

		appendLines(logs, "agent0: "+matching)
		if err := <-result; err != nil {
			t.Fatalf("wait failed after the target process logged the expected limits: %v", err)
		}
	})
}

func TestWaitForAgentConfigurationFailsWithoutMatch(t *testing.T) {
	for _, tt := range []struct {
		name    string
		signal  func(context.CancelFunc, chan struct{})
		cause   error
		elapsed time.Duration
	}{
		{name: "process exit", signal: func(_ context.CancelFunc, done chan struct{}) { close(done) }, cause: errProcessExited},
		{name: "startup canceled", signal: func(cancel context.CancelFunc, _ chan struct{}) { cancel() }, cause: context.Canceled},
		{name: "device readiness deadline", cause: context.DeadlineExceeded, elapsed: deviceWaitTimeout},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				logs := &logSink{}
				agent := &proc{name: "agent0", done: make(chan struct{})}
				limits := &ownsandv1.DeviceLimitsInput{VcpuCount: proto.Int32(8), MemoryMib: proto.Int64(16384), DiskGib: proto.Int64(100)}
				start := time.Now()
				result := make(chan error, 1)
				go func() { result <- logs.waitForAgentLimits(ctx, agent, "dev-1", limits) }()
				synctest.Wait()
				if tt.signal != nil {
					tt.signal(cancel, agent.done)
				}
				err := <-result
				if !errors.Is(err, tt.cause) {
					t.Fatalf("wait returned %v, want %v", err, tt.cause)
				}
				if elapsed := time.Since(start); elapsed != tt.elapsed {
					t.Errorf("wait failed after %v, want %v", elapsed, tt.elapsed)
				}
				for _, want := range []string{"dev-1", "agent0", "configuration received", "vcpu_limit=8", "memory_mib_limit=16384", "disk_gib_limit=100"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not contain %q", err, want)
					}
				}
			})
		})
	}
}
