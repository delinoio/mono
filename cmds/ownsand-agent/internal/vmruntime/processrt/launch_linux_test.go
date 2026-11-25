// SPDX-License-Identifier: FSL-1.1-ALv2

package processrt

import (
	"errors"
	"io"
	"log/slog"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

func TestHelperSignalFinishesBeforeReap(t *testing.T) {
	sending := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		if !channelClosed(release) {
			close(release)
		}
	}()
	sent := make(chan struct{})
	l := helperLifecycle{send: func(syscall.Signal) error {
		close(sending)
		<-release
		close(sent)
		return nil
	}}
	signalDone := make(chan error, 1)
	go func() { signalDone <- l.signal(sigStop) }()
	awaitHelperLifecycle(t, sending, "signal callback")

	observed := make(chan struct{})
	waitBeforeSend := make(chan bool, 1)
	go l.reap(func() error {
		close(observed)
		return nil
	}, func() {
		waitBeforeSend <- !channelClosed(sent)
	}, slog.Default())
	awaitHelperLifecycle(t, observed, "exit observation")
	select {
	case <-waitBeforeSend:
		t.Fatal("the reaper started the command wait while the signal was in progress")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := awaitHelperLifecycle(t, signalDone, "signal completion"); err != nil {
		t.Fatalf("admitted signal failed: %v", err)
	}
	if awaitHelperLifecycle(t, waitBeforeSend, "command wait") {
		t.Fatal("the command wait started before the signal callback returned")
	}
}

func TestHelperReapRejectsLaterSignals(t *testing.T) {
	l := helperLifecycle{send: func(syscall.Signal) error {
		t.Error("the signal callback ran after the command wait began")
		return nil
	}}
	waiting := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	go l.reap(func() error { return nil }, func() {
		close(waiting)
		<-release
	}, slog.Default())
	awaitHelperLifecycle(t, waiting, "command wait")

	signalDone := make(chan error, 1)
	go func() { signalDone <- l.signal(sigKill) }()
	if err := awaitHelperLifecycle(t, signalDone, "late signal result"); !errors.Is(err, errHelperExited) {
		t.Fatalf("signal during command wait = %v, want %v", err, errHelperExited)
	}
}

func TestHelperReapDisablesSignalsWhenOwnershipIsUncertain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		var signals int
		l := helperLifecycle{send: func(syscall.Signal) error {
			signals++
			return nil
		}}
		results := []error{syscall.EINTR, syscall.EINVAL, syscall.ECHILD}
		observations := 0
		waited := false
		l.reap(func() error {
			if observations == len(results) {
				t.Fatal("observation retried after ECHILD")
			}
			if observations == 1 {
				if err := l.signal(sigStop); err != nil {
					t.Errorf("signal after EINTR: %v", err)
				}
			}
			if observations == 2 {
				if err := l.signal(sigStop); !errors.Is(err, errHelperOwnershipUncertain) {
					t.Errorf("signal after failed observation = %v, want %v", err, errHelperOwnershipUncertain)
				}
			}
			err := results[observations]
			observations++
			return err
		}, func() {
			waited = true
			if err := l.signal(sigKill); !errors.Is(err, errHelperExited) {
				t.Errorf("signal during command wait = %v, want %v", err, errHelperExited)
			}
		}, logger)
		if observations != len(results) || !waited || signals != 1 {
			t.Fatalf("observations = %d, waited = %t, signals = %d; want 3, true, 1", observations, waited, signals)
		}
	})
}

func awaitHelperLifecycle[T any](t *testing.T, event <-chan T, name string) T {
	t.Helper()
	select {
	case value := <-event:
		return value
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
		var zero T
		return zero
	}
}

func channelClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
