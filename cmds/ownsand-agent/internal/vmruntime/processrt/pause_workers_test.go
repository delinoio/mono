// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package processrt

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/sandboxes"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestProcessPauseWaitsForAllWorkers(t *testing.T) {
	rt, mgr := newPauseManager(t)
	target := createManagedSandbox(t, rt, mgr)
	other := createManagedSandbox(t, rt, mgr)
	shell, child := startProgressWorker(t, target)
	startProgressWorker(t, other)

	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce sync.Once
	observed := make(map[int]bool)
	target.observeWorkerStopped = func(ctx context.Context, pid int) (bool, error) {
		stopped, err := processStopped(ctx, pid)
		if err != nil || !stopped {
			return stopped, err
		}
		observed[pid] = true
		if pid == child {
			enterOnce.Do(func() { close(entered) })
			select {
			case <-release:
			default:
				return false, nil
			}
		}
		return true, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() {
		cancel()
		awaitPause(t, done, "pause cleanup")
	}()
	var pauseErr error
	go func() {
		defer close(done)
		_, pauseErr = mgr.Pause(ctx, &ownsandv1.PauseSandboxCommand{SandboxId: target.id})
	}()
	select {
	case <-entered:
	case <-done:
		t.Fatalf("Pause completed before worker confirmation: %v", pauseErr)
	case <-time.After(8 * time.Second):
		t.Fatal("Pause did not reach the held worker observation")
	}

	assertManagedState(t, mgr, target, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	probeCtx, cancelProbe := context.WithTimeout(t.Context(), psTimeout)
	defer cancelProbe()
	for _, pid := range []int{target.helper.pid, shell, child} {
		state, err := processState(probeCtx, pid)
		if err != nil {
			t.Fatalf("PID %d after stop dispatch: probe failed: %v", pid, err)
		}
		if state == "" || state[0] != 'T' {
			t.Fatalf("PID %d after stop dispatch has state %q, want T", pid, state)
		}
	}
	assertWorkerProgress(t, other)
	select {
	case <-done:
		t.Fatalf("Pause completed with a worker observation pending: %v", pauseErr)
	default:
	}
	t.Log("Stop signals took effect. The pending child observation held the sandbox in RUNNING while the unrelated worker advanced.")

	close(release)
	awaitPause(t, done, "worker confirmation")
	if pauseErr != nil || !observed[shell] || !observed[child] {
		t.Fatalf("Pause = %v, observed PIDs %v, want confirmation of %d and %d", pauseErr, observed, shell, child)
	}
	assertManagedState(t, mgr, target, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED)
	assertWorkerProgress(t, other)
	if _, err := mgr.Resume(ctx, &ownsandv1.ResumeSandboxCommand{SandboxId: target.id}); err != nil {
		t.Fatal(err)
	}
	assertManagedState(t, mgr, target, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	assertWorkerProgress(t, target)
	t.Log("Confirming both members published PAUSED. Resume returned the target to RUNNING and its worker advanced.")
}

func TestProcessPauseRollsBackFailedWorkerConfirmation(t *testing.T) {
	probeErr := errors.New("worker state probe failed")
	for _, tc := range []struct {
		name          string
		cause         error
		callerTimeout time.Duration
	}{
		{"worker timeout", context.DeadlineExceeded, 9 * time.Second},
		{"caller deadline", context.DeadlineExceeded, 2 * time.Second},
		{"cancel", context.Canceled, 9 * time.Second},
		{"observation error", probeErr, 9 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, mgr := newPauseManager(t)
			target := createManagedSandbox(t, rt, mgr)
			shell, child := startProgressWorker(t, target)

			ctx, cancel := context.WithTimeout(context.Background(), tc.callerTimeout)
			defer cancel()
			callerDeadline, _ := ctx.Deadline()
			var observedErr error
			var observationDeadline time.Time
			target.observeWorkerStopped = func(ctx context.Context, pid int) (bool, error) {
				stopped, err := processStopped(ctx, pid)
				if err != nil || !stopped || pid != child {
					return stopped, err
				}
				observationDeadline, _ = ctx.Deadline()
				if tc.cause == probeErr {
					return false, probeErr
				}
				if tc.cause == context.Canceled {
					cancel()
				}
				<-ctx.Done()
				observedErr = ctx.Err()
				// A probe can complete successfully as its context ends.
				return true, nil
			}

			entry, err := mgr.Pause(ctx, &ownsandv1.PauseSandboxCommand{SandboxId: target.id})
			var cmdErr *sandboxes.CommandError
			if !errors.As(err, &cmdErr) || cmdErr.Code != ownsandv1.ErrorCode_ERROR_CODE_INTERNAL ||
				entry.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
				t.Fatalf("failed Pause = %v, %v, want RUNNING and an internal error", entry, err)
			}
			if tc.cause != probeErr && !errors.Is(observedErr, tc.cause) {
				t.Fatalf("worker observation context ended with %v, want %v", observedErr, tc.cause)
			}
			if tc.name == "worker timeout" && (observationDeadline.IsZero() || !observationDeadline.Before(callerDeadline)) {
				t.Fatalf("worker confirmation deadline %v did not shorten the caller deadline %v", observationDeadline, callerDeadline)
			}
			if tc.name == "caller deadline" && !observationDeadline.Equal(callerDeadline) {
				t.Fatalf("worker confirmation deadline %v, want caller deadline %v", observationDeadline, callerDeadline)
			}
			assertManagedState(t, mgr, target, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
			for _, pid := range []int{target.helper.pid, shell, child} {
				waitState(t, "worker confirmation rollback", pid, "S", "R")
			}
			assertWorkerProgress(t, target)

			target.observeWorkerStopped = nil
			if _, err := mgr.Pause(context.Background(), &ownsandv1.PauseSandboxCommand{SandboxId: target.id}); err != nil {
				t.Fatalf("retry Pause: %v", err)
			}
			assertManagedState(t, mgr, target, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED)
			if _, err := mgr.Resume(context.Background(), &ownsandv1.ResumeSandboxCommand{SandboxId: target.id}); err != nil {
				t.Fatal(err)
			}
			assertWorkerProgress(t, target)
			t.Log("Failed confirmation kept RUNNING and continued the helper and both workers. Retry paused successfully and resume restored progress.")
		})
	}
}

func TestProcessPauseAcceptsExitedWorkers(t *testing.T) {
	rt := newRuntime(t)
	target := createSandbox(t, rt).(*vm)
	if err := rt.Pause(context.Background(), target); err != nil {
		t.Fatalf("Pause without workers: %v", err)
	}
	if err := rt.Resume(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	_, child := startWorker(t, target)
	killed := false
	target.observeWorkerStopped = func(ctx context.Context, pid int) (bool, error) {
		if pid == child && !killed {
			killed = true
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
				return false, err
			}
			// Poll again after the kernel delivers SIGKILL.
			return false, nil
		}
		return processStopped(ctx, pid)
	}
	if err := rt.Pause(context.Background(), target); err != nil || !killed {
		t.Fatalf("Pause with worker exit = %v, killed = %t", err, killed)
	}
	waitState(t, "worker that exited during confirmation", child, "")
	// A listing with no members of a discovered group satisfies confirmation.
	if err := target.waitWorkersStopped(context.Background(), []int{-1}); err != nil {
		t.Fatalf("confirm an empty process group: %v", err)
	}
}

func newPauseManager(t *testing.T) (*Runtime, *sandboxes.Manager) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rt := newRuntime(t)
	mgr, err := sandboxes.New(context.Background(), sandboxes.Options{
		Store: st, Runtime: rt, Logger: slog.New(slog.NewTextHandler(t.Output(), nil)),
		FreeDisk: func() (int64, error) { return 1 << 50, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Close)
	return rt, mgr
}

func createManagedSandbox(t *testing.T, rt *Runtime, mgr *sandboxes.Manager) *vm {
	t.Helper()
	id, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	guestOS, baseImage := HostGuest()
	_, err = mgr.Create(context.Background(), &ownsandv1.CreateSandboxCommand{
		SandboxId: id, GuestOs: guestOS,
		Source:      &ownsandv1.CreateSandboxCommand_BaseImage{BaseImage: baseImage},
		Resources:   &ownsandv1.Resources{VcpuCount: 1, MemoryMib: 512, DiskGib: 1},
		NetworkMode: ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND,
		IdleTtl:     durationpb.New(time.Hour),
		MaxTtl:      durationpb.New(2 * time.Hour),
		MaxDeadline: timestamppb.New(time.Now().Add(2 * time.Hour)),
		Limits:      &ownsandv1.DeviceLimits{VcpuCount: 4, MemoryMib: 4096, DiskGib: 20, VcpuOvercommitPercent: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	target, err := rt.lookup(id)
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func startProgressWorker(t *testing.T, target *vm) (shell, child int) {
	t.Helper()
	return startWorkerCommand(t, target, `
		(while :; do printf x >> progress; sleep 0.05; done) &
		echo $! $$
		wait
	`)
}

func assertWorkerProgress(t *testing.T, target *vm) {
	t.Helper()
	path := filepath.Join(target.dir, rootDir, "progress")
	size := func() int64 {
		info, err := os.Stat(path)
		if err == nil {
			return info.Size()
		}
		if errors.Is(err, os.ErrNotExist) {
			return 0
		}
		t.Fatal(err)
		return 0
	}
	initial := size()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if size() > initial {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("worker in sandbox %s made no progress", target.id)
}

func assertManagedState(t *testing.T, mgr *sandboxes.Manager, target *vm, want ownsandv1.SandboxState) {
	t.Helper()
	if got, _ := mgr.State(target.id); got != want {
		t.Fatalf("sandbox %s state = %v, want %v", target.id, got, want)
	}
}

func awaitPause(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatalf("%s did not complete", what)
	}
}
