// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

// TestServeH2CAndShutdown checks that the server speaks HTTP/2 without TLS
// (prior knowledge), which the bidirectional streams need behind Caddy, and
// that a cancelled context drains it.
func TestServeH2CAndShutdown(t *testing.T) {
	const (
		requestTimeout = 5 * time.Second
		drainTimeout   = time.Second
		joinTimeout    = 5 * time.Second
	)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	srv := newServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Proto)
	}), logger)
	ctx, cancel := context.WithCancel(context.Background())
	drained := false
	done := make(chan error, 1)
	stopAndJoin := sync.OnceFunc(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
			if !drained {
				t.Error("beforeDrain did not run")
			}
		case <-time.After(joinTimeout):
			t.Errorf("serve did not stop within %s after cancellation", joinTimeout)
			_ = srv.Close()
			_ = listener.Close()
		}
	})
	t.Cleanup(stopAndJoin)

	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	client := &http.Client{
		Transport: &http.Transport{Protocols: &protocols},
		Timeout:   requestTimeout,
	}
	t.Cleanup(client.CloseIdleConnections)
	go func() {
		done <- serve(ctx, func() {}, srv, listener, logger, drainTimeout, func() { drained = true })
	}()

	resp, err := client.Get("http://" + listener.Addr().String() + "/")
	if err != nil {
		t.Fatalf("request h2c response: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if string(body) != "HTTP/2.0" {
		t.Fatalf("protocol = %q", body)
	}
	stopAndJoin()
}

// TestServerClosesConnectionWithBlockedWrites checks that the server bounds a
// socket write that blocks: a relay connection whose peer stopped draining
// its socket would otherwise keep its writer, and the Connect handler that
// waits for it, forever.
func TestServerClosesConnectionWithBlockedWrites(t *testing.T) {
	srv := newServer(http.NotFoundHandler(), slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if srv.HTTP2 == nil || srv.HTTP2.WriteByteTimeout != writeByteTimeout || writeByteTimeout <= 0 {
		t.Fatalf("HTTP2 config = %+v, want WriteByteTimeout %v", srv.HTTP2, writeByteTimeout)
	}
}
