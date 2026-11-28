// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

type fakeUsage struct {
	drain    func(ctx context.Context) error
	flush    func(ctx context.Context) error
	flushErr error
	calls    []string
}

func (f *fakeUsage) DrainUsage(ctx context.Context) error {
	f.calls = append(f.calls, "drain")
	return f.drain(ctx)
}

func (f *fakeUsage) FlushUsageFinal(ctx context.Context) error {
	f.calls = append(f.calls, "flush")
	if f.flush != nil {
		if err := f.flush(ctx); err != nil {
			return err
		}
	}
	return f.flushErr
}

// TestFinishUsageFlushesAfterDrain checks that the final flush runs after a
// successful drain and the shutdown reports no error.
func TestFinishUsageFlushesAfterDrain(t *testing.T) {
	f := &fakeUsage{drain: func(context.Context) error { return nil }}
	var logs bytes.Buffer
	if err := finishUsage(f, slog.New(slog.NewJSONHandler(&logs, nil)), time.Second, time.Second); err != nil {
		t.Fatalf("finishUsage = %v", err)
	}
	if got := strings.Join(f.calls, ","); got != "drain,flush" {
		t.Errorf("calls = %s, want drain,flush", got)
	}
	if strings.Contains(logs.String(), "usage_drain_incomplete") {
		t.Errorf("unexpected incomplete log: %s", logs.String())
	}
}

// TestFinishUsageBoundedWhenHandlerNeverExits checks that a drain that never
// completes ends within its bound, is logged and returned as an error, and
// that the final flush still runs.
func TestFinishUsageBoundedWhenHandlerNeverExits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const drainTimeout = 50 * time.Millisecond
		start := time.Now()
		f := &fakeUsage{drain: func(ctx context.Context) error {
			if err := ctx.Err(); err != nil {
				t.Fatalf("drain context already ended: %v", err)
			}
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("drain context has no deadline")
			}
			if got := deadline.Sub(start); got != drainTimeout {
				t.Fatalf("drain deadline is %s after start, want %s", got, drainTimeout)
			}

			timer := time.NewTimer(time.Second)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				if err := ctx.Err(); err != context.DeadlineExceeded {
					t.Fatalf("drain context ended with %v, want %v", err, context.DeadlineExceeded)
				}
				if elapsed := time.Since(start); elapsed != drainTimeout {
					t.Fatalf("drain context ended after %s, want %s", elapsed, drainTimeout)
				}
			case <-timer.C:
				t.Fatal("drain context remained active for 1s")
			}
			return errors.New("1 still running")
		}}
		var logs bytes.Buffer
		err := finishUsage(f, slog.New(slog.NewJSONHandler(&logs, nil)), 2*time.Second, drainTimeout)
		if !errors.Is(err, errUsageDrainIncomplete) {
			t.Fatalf("finishUsage = %v, want errUsageDrainIncomplete", err)
		}
		if got := strings.Join(f.calls, ","); got != "drain,flush" {
			t.Errorf("calls = %s, want drain,flush", got)
		}
		if !strings.Contains(logs.String(), "usage_drain_incomplete") {
			t.Errorf("missing usage_drain_incomplete log: %s", logs.String())
		}
	})
}

func TestFinishUsageReturnsFinalFlushError(t *testing.T) {
	drainErr := errors.New("requests still running")
	flushErr := errors.New("failed to write usage counters")
	tests := []struct {
		name       string
		drainErr   error
		wantErrors []error
		wantLogs   []string
	}{
		{
			name:       "flush failure",
			wantErrors: []error{flushErr},
			wantLogs:   []string{"usage_final_flush_failed"},
		},
		{
			name:       "drain and flush failures",
			drainErr:   drainErr,
			wantErrors: []error{errUsageDrainIncomplete, drainErr, flushErr},
			wantLogs:   []string{"usage_drain_incomplete", "usage_final_flush_failed"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeUsage{
				drain:    func(context.Context) error { return tt.drainErr },
				flushErr: flushErr,
			}
			var logs bytes.Buffer
			err := finishUsage(f, slog.New(slog.NewJSONHandler(&logs, nil)), time.Second, time.Second)
			for _, want := range tt.wantErrors {
				if !errors.Is(err, want) {
					t.Errorf("finishUsage = %v, want it to wrap %v", err, want)
				}
			}
			if got := strings.Join(f.calls, ","); got != "drain,flush" {
				t.Errorf("calls = %s, want drain,flush", got)
			}
			for _, msg := range tt.wantLogs {
				if !strings.Contains(logs.String(), msg) {
					t.Errorf("missing %s log: %s", msg, logs.String())
				}
			}
		})
	}
}
