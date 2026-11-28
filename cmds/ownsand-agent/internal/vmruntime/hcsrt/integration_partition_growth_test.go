// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

const (
	// The 40 GiB fixture disk is larger than the default 32 GiB image.
	windowsDiskGiB         = 40
	windowsPartitionMin    = (windowsDiskGiB - 2) << 30
	integrationExecTimeout = 60 * time.Second
)

func waitForWindowsPartitionGrowth(timeout time.Duration, probe func(context.Context) (string, error)) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	lastSize := "unavailable"

	for {
		probeCtx, cancelProbe := context.WithTimeout(ctx, integrationExecTimeout)
		probeDeadline, _ := probeCtx.Deadline()
		if !time.Now().Before(deadline) {
			cancelProbe()
			break
		}
		out, err := probe(probeCtx)
		size, parseErr := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
		observed := time.Now()
		cancelProbe()
		// Keep complete late samples for the timeout diagnostic.
		if err == nil && parseErr == nil {
			lastSize = strconv.FormatInt(size, 10) + " bytes"
		}
		if !observed.Before(deadline) {
			break
		}
		if observed.Before(probeDeadline) {
			if err != nil {
				return 0, err
			}
			if parseErr == nil && size > windowsPartitionMin {
				return size, nil
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
	return 0, fmt.Errorf("C: did not grow to the %d GiB disk before the deadline (last observed size: %s): %w", windowsDiskGiB, lastSize, context.DeadlineExceeded)
}

func TestWindowsPartitionGrowthResultTiming(t *testing.T) {
	const timeout = time.Second
	for _, tc := range []struct {
		name  string
		delay time.Duration
	}{
		{name: "before deadline", delay: timeout - time.Nanosecond},
		{name: "at deadline", delay: timeout},
		{name: "after deadline", delay: timeout + time.Nanosecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				size, err := waitForWindowsPartitionGrowth(timeout, func(context.Context) (string, error) {
					time.Sleep(tc.delay)
					return strconv.FormatInt(windowsPartitionMin+1, 10), nil
				})
				if tc.delay < timeout {
					if err != nil || size != windowsPartitionMin+1 {
						t.Fatalf("got %d bytes and %v, want %d bytes", size, err, windowsPartitionMin+1)
					}
					return
				}
				wantSize := fmt.Sprintf("last observed size: %d bytes", windowsPartitionMin+1)
				if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), wantSize) {
					t.Fatalf("got %d bytes and %v, want a growth timeout with %q", size, err, wantSize)
				}
			})
		})
	}
}

func TestWindowsPartitionGrowthExpiredBeforeFirstProbe(t *testing.T) {
	_, err := waitForWindowsPartitionGrowth(0, func(context.Context) (string, error) {
		t.Fatal("started a probe after the growth deadline")
		return "", nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want a growth timeout", err)
	}
}

func TestWindowsPartitionGrowthDeadlineDuringRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const timeout = 6 * time.Second
		start := time.Now()
		probes := 0
		_, err := waitForWindowsPartitionGrowth(timeout, func(context.Context) (string, error) {
			probes++
			time.Sleep(timeout - time.Second)
			return strconv.FormatInt(windowsPartitionMin, 10), nil
		})
		if elapsed := time.Since(start); elapsed != timeout || probes != 1 {
			t.Fatalf("got %v and %d probes, want %v and 1 probe", elapsed, probes, timeout)
		}
		wantSize := fmt.Sprintf("last observed size: %d bytes", windowsPartitionMin)
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), wantSize) {
			t.Fatalf("got %v, want a growth timeout with %q", err, wantSize)
		}
	})
}

func TestWindowsPartitionGrowthProbeDeadline(t *testing.T) {
	for _, tc := range []struct {
		name        string
		timeout     time.Duration
		probeBudget time.Duration
	}{
		{name: "growth deadline", timeout: 6 * time.Second, probeBudget: time.Second},
		{name: "exec cap", timeout: 2 * time.Minute, probeBudget: 60 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				probes := 0
				_, err := waitForWindowsPartitionGrowth(tc.timeout, func(ctx context.Context) (string, error) {
					probes++
					switch probes {
					case 1:
						return strconv.FormatInt(windowsPartitionMin, 10), nil
					case 2:
						probeStart := time.Now()
						<-ctx.Done()
						if elapsed := time.Since(probeStart); elapsed != tc.probeBudget {
							t.Fatalf("probe ended after %v, want %v", elapsed, tc.probeBudget)
						}
						return "", ctx.Err()
					default:
						return strconv.FormatInt(windowsPartitionMin+1, 10), nil
					}
				})
				if tc.probeBudget == time.Second {
					wantSize := fmt.Sprintf("last observed size: %d bytes", windowsPartitionMin)
					if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), wantSize) {
						t.Fatalf("got %v, want a growth timeout with %q", err, wantSize)
					}
					if elapsed := time.Since(start); elapsed != tc.timeout || probes != 2 {
						t.Fatalf("got %v and %d probes, want %v and 2 probes", elapsed, probes, tc.timeout)
					}
				} else if elapsed := time.Since(start); err != nil || elapsed != 70*time.Second || probes != 3 {
					t.Fatalf("got %v after %v and %d probes, want success after 70s and 3 probes", err, elapsed, probes)
				}
			})
		})
	}
}
