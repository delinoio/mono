// SPDX-License-Identifier: FSL-1.1-ALv2

package lease

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/tide-api/internal/db/dbtest"
)

func TestSupervisorStartStop(t *testing.T) {
	s := &Supervisor{Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), restartMin: time.Millisecond}
	var running, runs atomic.Int32
	s.Add(ComponentFunc{ComponentName: "blocking", RunFunc: func(ctx context.Context) error {
		runs.Add(1)
		running.Add(1)
		defer running.Add(-1)
		<-ctx.Done()
		return nil
	}})
	var flaky atomic.Int32
	s.Add(ComponentFunc{ComponentName: "flaky", RunFunc: func(ctx context.Context) error {
		if flaky.Add(1) < 3 {
			return errors.New("boom")
		}
		<-ctx.Done()
		return nil
	}})
	s.Stop() // Stop before Start does nothing.
	s.Start()
	s.Start() // A second Start does nothing.
	deadline := time.Now().Add(5 * time.Second)
	for flaky.Load() < 3 || running.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("flaky runs %d, running %d", flaky.Load(), running.Load())
		}
		time.Sleep(time.Millisecond)
	}
	s.Stop()
	if running.Load() != 0 {
		t.Fatal("Stop returned before the component")
	}
	// A new ownership period starts the components again.
	s.Start()
	for runs.Load() != 2 {
		if time.Now().After(deadline) {
			t.Fatalf("runs = %d", runs.Load())
		}
		time.Sleep(time.Millisecond)
	}
	s.Stop()
}

// TestSupervisorWithLease checks that the lease starts the components when
// it is acquired and stops them when it is released.
func TestSupervisorWithLease(t *testing.T) {
	database := dbtest.New(t)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	l := &Lease{DB: database, InstanceID: "a", Logger: logger, renewEvery: 20 * time.Millisecond, poll: 5 * time.Millisecond}
	s := &Supervisor{Logger: logger}
	started := make(chan struct{}, 1)
	var running atomic.Bool
	s.Add(ComponentFunc{ComponentName: "c", RunFunc: func(ctx context.Context) error {
		running.Store(true)
		started <- struct{}{}
		<-ctx.Done()
		running.Store(false)
		return nil
	}})
	l.OnAcquire = func(context.Context) { s.Start() }
	l.OnLose = s.Stop
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); l.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("component did not start")
	}
	cancel()
	<-done
	if running.Load() {
		t.Fatal("component still runs after the release")
	}
}
