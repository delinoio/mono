// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestServeH2CAndShutdown checks that the server speaks HTTP/2 without TLS
// (prior knowledge), which the bidirectional streams need behind Caddy, and
// that a cancelled context drains it.
func TestServeH2CAndShutdown(t *testing.T) {
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
	go func() {
		done <- serve(ctx, func() {}, srv, listener, logger, time.Second, func() { drained = true })
	}()

	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	client := &http.Client{Transport: &http.Transport{Protocols: &protocols}}
	resp, err := client.Get("http://" + listener.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "HTTP/2.0" {
		t.Fatalf("protocol = %q", body)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
	if !drained {
		t.Fatal("beforeDrain did not run")
	}
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
