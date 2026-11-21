// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

func serveLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestServeExitsWithFailureOnTerminalTunnelError(t *testing.T) {
	fatal := make(chan error, 1)
	started := make(chan struct{})
	stopped := make(chan struct{})
	result := make(chan int, 1)
	go func() {
		result <- serve(context.Background(), serveLogger(), fatal, func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			close(stopped)
			return nil
		})
	}()
	<-started
	fatal <- errors.New("tunnel: TAP read: gone")
	select {
	case code := <-result:
		if code != 1 {
			t.Fatalf("exit code %d, want 1", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after a terminal tunnel error")
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the guest service was not asked to stop")
	}
}

func TestServeWithoutTunnelFollowsTheService(t *testing.T) {
	// A nil tunnel channel never fires.
	if code := serve(context.Background(), serveLogger(), nil, func(context.Context) error { return nil }); code != 0 {
		t.Errorf("clean stop: exit code %d, want 0", code)
	}
	if code := serve(context.Background(), serveLogger(), nil, func(context.Context) error { return errors.New("boom") }); code != 1 {
		t.Errorf("service error: exit code %d, want 1", code)
	}
}

func TestServeIgnoresAQuietTunnel(t *testing.T) {
	fatal := make(chan error, 1)
	if code := serve(context.Background(), serveLogger(), fatal, func(context.Context) error { return nil }); code != 0 {
		t.Errorf("exit code %d, want 0", code)
	}
}
