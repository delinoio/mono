// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/internal/telemetry"
	"github.com/delinoio/mono/servers/sandrun-api/internal/config"
	"github.com/delinoio/mono/servers/sandrun-api/internal/secrets"
)

func TestCanceledStartup(t *testing.T) {
	for _, tc := range []struct {
		name          string
		enableMetrics bool
		cancelAfter   int
	}{
		{name: "before first bind", enableMetrics: true, cancelAfter: 0},
		{name: "after public bind without metrics", cancelAfter: 1},
		{name: "after public bind with metrics configured", enableMetrics: true, cancelAfter: 1},
		{name: "after metrics bind", enableMetrics: true, cancelAfter: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			tel, err := telemetry.New(t.Context(), telemetry.Options{
				ServiceName: "sandrun-api",
				Stdout:      &logs,
				LookupEnv:   func(string) (string, bool) { return "", false },
			})
			if err != nil {
				t.Fatal(err)
			}
			defer tel.Shutdown(context.Background())

			cfg := config.Config{Addr: "127.0.0.1:0", SecretKeys: &secrets.Keyring{}}
			if tc.enableMetrics {
				cfg.MetricsAddr = "127.0.0.1:0"
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancelAfter == 0 {
				cancel()
			}
			var listeners []*startupListener
			listen := func(_, _ string) (net.Listener, error) {
				listener := bindStartupListener(t)
				listeners = append(listeners, listener)
				if len(listeners) == tc.cancelAfter {
					cancel()
				}
				return listener, nil
			}
			var workerCalls atomic.Int32
			stopCalls := 0
			err = startAndServe(ctx, func() { stopCalls++ }, cfg, tel, http.NotFoundHandler(),
				listen, func(context.Context) { workerCalls.Add(1) })
			if err != nil {
				t.Errorf("canceled startup returned %v, want nil", err)
			}
			if stopCalls != 1 {
				t.Errorf("stop calls = %d, want 1", stopCalls)
			}
			if calls := workerCalls.Load(); calls != 0 {
				t.Errorf("lease worker calls = %d, want 0", calls)
			}
			if len(listeners) != tc.cancelAfter {
				t.Errorf("listener binds = %d, want %d", len(listeners), tc.cancelAfter)
			}
			if logOutput := logs.String(); strings.Contains(logOutput, "server_started") ||
				strings.Contains(logOutput, "server_shutdown_started") {
				t.Errorf("logged serving or HTTP drain during canceled startup: %s", logOutput)
			}
			for _, listener := range listeners {
				listener.checkClosedWithoutServing(t)
			}
		})
	}
}

// TestServeH2CAndShutdown checks that the server speaks HTTP/2 without TLS
// (prior knowledge), which gRPC clients of the server streams need behind Caddy, and
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
		case <-time.After(joinTimeout):
			_ = srv.Close()
			_ = listener.Close()
			select {
			case err := <-done:
				t.Errorf("serve exceeded the shutdown deadline and exited after force close (result: %v)", err)
			case <-time.After(drainTimeout):
				t.Error("serve exceeded the shutdown deadline and did not exit after force close")
				return
			}
		}
		if !drained {
			t.Error("beforeDrain did not run")
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
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	stopAndJoin()
	if string(body) != "HTTP/2.0" {
		t.Fatalf("protocol = %q", body)
	}
}

var errPublicAccept = errors.New("permanent public accept failure")

// After one real connection, the accept loop waits for a permanent error.
type failingPublicListener struct {
	net.Listener
	accepted bool
	fail     <-chan struct{}
	closed   chan struct{}
}

func (l *failingPublicListener) Accept() (net.Conn, error) {
	if !l.accepted {
		l.accepted = true
		return l.Listener.Accept()
	}
	<-l.fail
	return nil, errPublicAccept
}

func (l *failingPublicListener) Close() error {
	err := l.Listener.Close()
	close(l.closed)
	return err
}

func TestServeDrainsAfterPublicListenerFailure(t *testing.T) {
	for _, tc := range []struct {
		name        string
		signalFirst bool
		forceClose  bool
		h2c         bool
	}{
		{name: "active request completes"},
		{name: "active request exceeds drain deadline", forceClose: true},
		{name: "cancellation overlaps public listener failure", signalFirst: true},
		{name: "active h2c request completes", h2c: true},
		{name: "active h2c request exceeds drain deadline", forceClose: true, h2c: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			wait := func(done <-chan struct{}, event string) {
				t.Helper()
				select {
				case <-done:
				case <-ctx.Done():
					t.Fatalf("timed out waiting for %s", event)
				}
			}
			tcp, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = tcp.Close() })
			failAccept := make(chan struct{})
			failListener := sync.OnceFunc(func() { close(failAccept) })
			defer failListener()
			listener := &failingPublicListener{Listener: tcp, fail: failAccept, closed: make(chan struct{})}

			requestStarted := make(chan struct{})
			finishRequest := make(chan struct{})
			releaseRequest := sync.OnceFunc(func() { close(finishRequest) })
			defer releaseRequest()
			requestCanceled := make(chan struct{})
			logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
			srv := newServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(requestStarted)
				select {
				case <-finishRequest:
					_, _ = io.WriteString(w, "drained")
				case <-r.Context().Done():
					close(requestCanceled)
				}
			}), logger)
			shutdownStarted := make(chan struct{})
			srv.RegisterOnShutdown(func() { close(shutdownStarted) })
			serveCtx, signal := context.WithCancel(ctx)
			defer signal()
			signalStopped := make(chan struct{})
			workersStopping := make(chan struct{})
			workersStopped := make(chan struct{})
			releaseWorkers := sync.OnceFunc(func() { close(workersStopped) })
			defer releaseWorkers()
			drainTimeout := 2 * time.Second
			if tc.forceClose {
				drainTimeout = 30 * time.Millisecond
			}
			done := make(chan error, 1)
			go func() {
				done <- serve(serveCtx, func() { close(signalStopped) }, srv, listener, logger, drainTimeout, func() {
					close(workersStopping)
					<-workersStopped
				})
			}()

			request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+tcp.Addr().String(), nil)
			if err != nil {
				t.Fatal(err)
			}
			var protocols http.Protocols
			protocols.SetHTTP1(!tc.h2c)
			protocols.SetUnencryptedHTTP2(tc.h2c)
			client := &http.Client{Transport: &http.Transport{Protocols: &protocols}}
			defer client.CloseIdleConnections()
			response := make(chan error, 1)
			go func() {
				resp, err := client.Do(request)
				if err == nil {
					body, readErr := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					err = readErr
					if err == nil && (resp.StatusCode != http.StatusOK || string(body) != "drained") {
						err = fmt.Errorf("unexpected response: %s %q", resp.Status, body)
					}
				}
				response <- err
			}()
			wait(requestStarted, "active HTTP request")
			if tc.signalFirst {
				signal()
				wait(signalStopped, "signal handler removal")
				wait(workersStopping, "lease worker shutdown after cancellation")
			}
			failListener()
			wait(listener.closed, "Serve closing the failed listener")
			select {
			case <-workersStopping:
			case err := <-done:
				t.Fatalf("serve returned before stopping lease workers: %v", err)
			case <-ctx.Done():
				t.Fatal("serve did not stop lease workers")
			}
			select {
			case <-shutdownStarted:
				t.Fatal("HTTP drain started before lease workers stopped")
			default:
			}
			if !tc.signalFirst {
				select {
				case <-signalStopped:
					t.Fatal("listener failure removed the signal handler")
				default:
				}
			}
			releaseWorkers()
			wait(shutdownStarted, "HTTP drain")
			if !tc.forceClose {
				select {
				case err := <-done:
					t.Fatalf("serve returned with an active request: %v", err)
				default:
				}
				releaseRequest()
			}

			select {
			case err := <-response:
				if tc.forceClose && (err == nil || ctx.Err() != nil) {
					t.Fatalf("HTTP request error = %v, want a connection error before the client deadline", err)
				}
				if !tc.forceClose && err != nil {
					t.Fatalf("active request failed during drain: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("no HTTP response or connection close")
			}
			if tc.forceClose {
				wait(requestCanceled, "request context cancellation after forced close")
			}
			select {
			case err := <-done:
				if !errors.Is(err, errPublicAccept) {
					t.Fatalf("serve error = %v, want the original listener error", err)
				}
			case <-ctx.Done():
				t.Fatal("serve did not return after draining")
			}
		})
	}
}

// TestServerClosesConnectionWithBlockedWrites checks that the server bounds a
// socket write that blocks: a server stream whose peer stopped reading
// would otherwise keep its handler forever.
func TestServerClosesConnectionWithBlockedWrites(t *testing.T) {
	srv := newServer(http.NotFoundHandler(), slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if srv.HTTP2 == nil || srv.HTTP2.WriteByteTimeout != writeByteTimeout || writeByteTimeout <= 0 {
		t.Fatalf("HTTP2 config = %+v, want WriteByteTimeout %v", srv.HTTP2, writeByteTimeout)
	}
}

// TestHookMode: the binary is the spawn-runner hook under the hook name
// (the link in the hooks directory) or with it as the first argument.
func TestHookMode(t *testing.T) {
	for args, want := range map[string]bool{
		"/tmp/tc1/hooks/spawn-runner":               true,
		"/usr/local/bin/sandrun-api spawn-runner":   true,
		"/usr/local/bin/sandrun-api":                false,
		"/usr/local/bin/sandrun-api serve --x=y":    false,
		"spawn-runner-other":                        false,
		"/usr/local/bin/sandrun-api x spawn-runner": false,
	} {
		if got := hookMode(strings.Fields(args)); got != want {
			t.Errorf("%q: %v", args, got)
		}
	}
}
