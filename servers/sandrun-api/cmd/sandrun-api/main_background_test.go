// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var errMetricsAccept = errors.New("permanent metrics accept failure")

type startupListener struct {
	net.Listener
	accepts atomic.Int32
	closes  atomic.Int32
}

func bindStartupListener(t *testing.T) *startupListener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return &startupListener{Listener: listener}
}

func (l *startupListener) Accept() (net.Conn, error) {
	l.accepts.Add(1)
	return nil, errors.New("unexpected Accept during canceled startup")
}

func (l *startupListener) Close() error {
	l.closes.Add(1)
	return l.Listener.Close()
}

func (l *startupListener) checkClosedWithoutServing(t *testing.T) {
	t.Helper()
	if accepts := l.accepts.Load(); accepts != 0 {
		t.Errorf("listener %s Accept calls = %d, want 0", l.Addr(), accepts)
	}
	if closes := l.closes.Load(); closes != 1 {
		t.Errorf("listener %s Close calls = %d, want 1", l.Addr(), closes)
	}
	if conn, err := net.DialTimeout("tcp", l.Addr().String(), time.Second); err == nil {
		_ = conn.Close()
		t.Errorf("listener %s still accepts connections", l.Addr())
	}
}

func TestCanceledServingEntry(t *testing.T) {
	for _, mode := range []string{"serve", "runServers without metrics", "runServers with metrics"} {
		t.Run(mode, func(t *testing.T) {
			public := bindStartupListener(t)
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			srv := newServer(http.NotFoundHandler(), logger)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			var steps []string
			stop := func() { steps = append(steps, "stop") }
			beforeDrain := func() { steps = append(steps, "beforeDrain") }

			var err error
			switch mode {
			case "serve":
				err = serve(ctx, stop, srv, public, logger, time.Second, beforeDrain)
			case "runServers without metrics":
				err = runServers(ctx, stop, srv, public, logger, time.Second, beforeDrain, nil, nil)
			case "runServers with metrics":
				metrics := bindStartupListener(t)
				err = runServers(ctx, stop, srv, public, logger, time.Second, beforeDrain,
					&http.Server{Handler: http.NotFoundHandler()}, metrics)
				metrics.checkClosedWithoutServing(t)
			}

			if err != nil {
				t.Errorf("canceled serving entry returned %v, want nil", err)
			}
			if got := strings.Join(steps, ", "); got != "stop, beforeDrain" {
				t.Errorf("cleanup steps = %q, want stop, beforeDrain", got)
			}
			if strings.Contains(logs.String(), "server_shutdown_started") {
				t.Errorf("logged HTTP drain before serving: %s", &logs)
			}
			public.checkClosedWithoutServing(t)
		})
	}
}

type failingMetricsListener struct {
	net.Listener
	fail   <-chan struct{}
	closed chan struct{}
}

func (l *failingMetricsListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	select {
	case <-l.fail:
		if conn != nil {
			_ = conn.Close()
		}
		return nil, errMetricsAccept
	default:
		return conn, err
	}
}

func (l *failingMetricsListener) Close() error {
	err := l.Listener.Close()
	close(l.closed)
	return err
}

func bindLoopback(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

func checkHTTPStatusOK(t *testing.T, listener net.Listener, path string, timeout time.Duration) {
	t.Helper()
	client := &http.Client{Timeout: timeout, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Get("http://" + listener.Addr().String() + path)
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read %s response: %v", path, readErr)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s status = %d, want 200", path, resp.StatusCode)
	}
}

func TestRunServersShutdown(t *testing.T) {
	for _, tt := range []struct {
		name           string
		metricsEnabled bool
		metricsFailure bool
		signalFirst    bool
	}{
		{name: "metrics failure", metricsEnabled: true, metricsFailure: true},
		{name: "signal overlaps metrics failure", metricsEnabled: true, metricsFailure: true, signalFirst: true},
		{name: "signal with metrics", metricsEnabled: true, signalFirst: true},
		{name: "signal without metrics", signalFirst: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const timeout = 5 * time.Second
			wait := func(done <-chan struct{}, event string) {
				t.Helper()
				select {
				case <-done:
				case <-time.After(timeout):
					t.Fatalf("timed out waiting for %s", event)
				}
			}
			ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

			public := bindLoopback(t)
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			srv := newServer(ok, logger)

			failMetrics := make(chan struct{})
			var metricsListener net.Listener
			var failedMetrics *failingMetricsListener
			var metricsSrv *http.Server
			if tt.metricsEnabled {
				failedMetrics = &failingMetricsListener{Listener: bindLoopback(t), fail: failMetrics, closed: make(chan struct{})}
				metricsListener = failedMetrics
				metricsSrv = &http.Server{Handler: ok}
			}

			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			stoppingWorkers := make(chan struct{})
			workersStopped := make(chan struct{})
			finishWorkers := make(chan struct{})
			releaseWorkers := sync.OnceFunc(func() { close(finishWorkers) })
			t.Cleanup(releaseWorkers)
			signalStopped := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				done <- runServers(ctx, func() { close(signalStopped) }, srv, public, logger, timeout, func() {
					close(stoppingWorkers)
					<-finishWorkers
					close(workersStopped)
				}, metricsSrv, metricsListener)
			}()

			checkHTTPStatusOK(t, public, "/healthz", timeout)
			if tt.metricsEnabled {
				checkHTTPStatusOK(t, metricsListener, "/metrics", timeout)
			}
			if tt.signalFirst {
				cancel()
				wait(stoppingWorkers, "worker shutdown after signal")
			}
			if tt.metricsFailure {
				close(failMetrics)
				conn, err := net.DialTimeout("tcp", metricsListener.Addr().String(), timeout)
				if err != nil {
					t.Fatal(err)
				}
				_ = conn.Close()
				wait(failedMetrics.closed, "metrics Serve closing the listener after the Accept error")
			}
			wait(stoppingWorkers, "worker shutdown")
			if tt.signalFirst {
				wait(signalStopped, "signal handler removal")
			} else {
				select {
				case <-signalStopped:
					t.Fatal("metrics failure removed the signal handler before a termination signal")
				default:
				}
				cancel()
			}
			// A fresh public connection still works while the worker and
			// lease callback is blocked. The HTTP drain must wait for it.
			checkHTTPStatusOK(t, public, "/healthz", timeout)
			releaseWorkers()

			var err error
			select {
			case err = <-done:
			case <-time.After(timeout):
				t.Fatal("runServers did not return")
			}
			wait(workersStopped, "worker shutdown completion")
			if tt.metricsFailure {
				if !errors.Is(err, errMetricsAccept) {
					t.Fatalf("runServers error = %v, want the original metrics error", err)
				}
				if !strings.Contains(logs.String(), "metrics_server_failed") || !strings.Contains(logs.String(), errMetricsAccept.Error()) {
					t.Fatalf("missing metrics error in logs: %s", logs.String())
				}
			} else if err != nil {
				t.Fatalf("runServers error = %v, want nil", err)
			} else if strings.Contains(logs.String(), "metrics_server_failed") || !strings.Contains(logs.String(), "server_shutdown_completed") {
				t.Fatalf("unexpected clean shutdown logs: %s", &logs)
			}
			if conn, err := net.DialTimeout("tcp", public.Addr().String(), timeout); err == nil {
				_ = conn.Close()
				t.Fatal("public listener still accepts connections after shutdown")
			}
		})
	}
}

type temporaryAcceptError struct{}

func (temporaryAcceptError) Error() string   { return "temporary accept failure" }
func (temporaryAcceptError) Timeout() bool   { return false }
func (temporaryAcceptError) Temporary() bool { return true }

type temporaryAcceptListener struct {
	net.Listener
	failed bool
}

func (l *temporaryAcceptListener) Accept() (net.Conn, error) {
	if !l.failed {
		l.failed = true
		return nil, temporaryAcceptError{}
	}
	return l.Listener.Accept()
}

func TestRunServersAcceptErrors(t *testing.T) {
	for _, tc := range []struct {
		name          string
		publicFailure bool
		temporary     bool
	}{
		{name: "metrics Accept fails after shutdown closes the listener"},
		{name: "public failure joins metrics failure during shutdown", publicFailure: true},
		{name: "both listeners retry temporary failures and shut down cleanly", temporary: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const timeout = 5 * time.Second
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			waitForFailure := func(l *acceptFailureListener) {
				t.Helper()
				select {
				case <-l.failureReady:
				case <-time.After(timeout):
					t.Fatalf("no pending Accept failure on %s", l.Addr())
				}
			}
			public, metrics := bindLoopback(t), bindLoopback(t)
			failPublic := make(chan struct{})
			var failedPublic, failedMetrics *acceptFailureListener
			if tc.publicFailure {
				failedPublic = newAcceptFailureListener(t, public, errPublicAccept, failPublic)
				public = failedPublic
			}
			if tc.temporary {
				public = &temporaryAcceptListener{Listener: public}
				metrics = &temporaryAcceptListener{Listener: metrics}
			} else {
				failedMetrics = newAcceptFailureListener(t, metrics, errMetricsAccept, nil)
				metrics = failedMetrics
			}

			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
			srv := newServer(ok, logger)
			metricsSrv := &http.Server{Handler: ok, ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelError)}
			done := make(chan error, 1)
			go func() {
				done <- runServers(ctx, func() {}, srv, public, logger, timeout, func() {}, metricsSrv, metrics)
			}()

			checkHTTPStatusOK(t, public, "/healthz", timeout)
			checkHTTPStatusOK(t, metrics, "/metrics", timeout)
			if failedMetrics != nil {
				waitForFailure(failedMetrics)
			}
			if failedPublic != nil {
				waitForFailure(failedPublic)
				close(failPublic)
			} else {
				cancel()
			}

			var err error
			select {
			case err = <-done:
			case <-time.After(timeout):
				t.Fatal("runServers did not return")
			}
			if tc.temporary {
				if err != nil {
					t.Fatalf("runServers error = %v, want nil", err)
				}
				if strings.Contains(logs.String(), "metrics_server_failed") || !strings.Contains(logs.String(), "server_shutdown_completed") {
					t.Fatalf("unexpected clean shutdown logs: %s", &logs)
				}
				return
			}
			if !errors.Is(err, errMetricsAccept) || (tc.publicFailure && !errors.Is(err, errPublicAccept)) {
				t.Fatalf("runServers error = %v, want metrics error and public error when both fail", err)
			}
			if !strings.Contains(logs.String(), "metrics_server_failed") || !strings.Contains(logs.String(), errMetricsAccept.Error()) {
				t.Fatalf("missing metrics error in logs: %s", &logs)
			}
		})
	}
}
