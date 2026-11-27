// SPDX-License-Identifier: FSL-1.1-ALv2

package vzrt_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

const integrationExecTimeout = 60 * time.Second

// waitForOutput polls until a probe returns the marker before the deadline.
func waitForOutput(want string, timeout time.Duration, probe func(context.Context) (string, error)) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	var lastOutput string
	for {
		probeCtx, cancelProbe := context.WithTimeout(ctx, integrationExecTimeout)
		if !time.Now().Before(deadline) {
			cancelProbe()
			break
		}
		out, err := probe(probeCtx)
		timely := time.Now().Before(deadline)
		cancelProbe()
		if out != "" {
			lastOutput = out
		}
		if !timely {
			break
		}
		if err != nil {
			return err
		}
		if strings.Contains(out, want) {
			return nil
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("wait for %q: %w (last output %q)", want, context.DeadlineExceeded, lastOutput)
}

func TestWaitForOutputMarkerTiming(t *testing.T) {
	for _, tc := range []struct {
		name  string
		delay time.Duration
	}{
		{name: "before deadline", delay: time.Second - time.Nanosecond},
		{name: "at deadline", delay: time.Second},
		{name: "after deadline", delay: time.Second + time.Nanosecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const timeout = time.Second
				err := waitForOutput("ready", timeout, func(context.Context) (string, error) {
					time.Sleep(tc.delay)
					return "ready", nil
				})
				if tc.delay < timeout {
					if err != nil {
						t.Fatal(err)
					}
					return
				}
				if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), `last output "ready"`) {
					t.Fatalf("got %v, want deadline error with the late marker", err)
				}
			})
		})
	}
}

func TestWaitForOutputExpiredBeforeFirstProbe(t *testing.T) {
	err := waitForOutput("ready", 0, func(context.Context) (string, error) {
		t.Fatal("started a probe with an expired polling deadline")
		return "", nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want deadline error", err)
	}
}

func TestWaitForOutputDeadlineDuringBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// The deadline interrupts the second retry delay.
		const timeout = 3 * time.Second
		start := time.Now()
		probes := 0
		err := waitForOutput("ready", timeout, func(context.Context) (string, error) {
			probes++
			if probes == 1 {
				return "starting", nil
			}
			return "", nil
		})
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), `last output "starting"`) {
			t.Fatalf("got %v, want deadline error with the last available output", err)
		}
		if elapsed := time.Since(start); elapsed != timeout || probes != 2 {
			t.Fatalf("got %v and %d probes, want %v and 2 probes", elapsed, probes, timeout)
		}
	})
}

func TestWaitForOutputProbeDeadline(t *testing.T) {
	for _, tc := range []struct {
		name        string
		timeout     time.Duration
		probeBudget time.Duration
	}{
		{name: "polling deadline", timeout: 7 * time.Second, probeBudget: 7 * time.Second},
		{name: "exec cap", timeout: 2 * time.Minute, probeBudget: 60 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				probes := 0
				err := waitForOutput("ready", tc.timeout, func(ctx context.Context) (string, error) {
					probes++
					if probes > 1 {
						return "ready", nil
					}
					deadline, ok := ctx.Deadline()
					if !ok || deadline.Sub(start) != tc.probeBudget {
						t.Fatalf("probe deadline = %v, want %v", deadline, start.Add(tc.probeBudget))
					}
					<-ctx.Done()
					if ctx.Err() != context.DeadlineExceeded || time.Since(start) != tc.probeBudget {
						t.Fatalf("probe ended after %v with %v", time.Since(start), ctx.Err())
					}
					return "in flight", nil
				})
				if tc.probeBudget == tc.timeout {
					if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), `last output "in flight"`) || probes != 1 {
						t.Fatalf("got %v after %d probes, want deadline error with partial output after one probe", err, probes)
					}
				} else if err != nil || probes != 2 {
					t.Fatalf("got %v after %d probes, want readiness on the second probe", err, probes)
				}
			})
		})
	}
}
