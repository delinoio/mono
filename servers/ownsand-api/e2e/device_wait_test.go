// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
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
