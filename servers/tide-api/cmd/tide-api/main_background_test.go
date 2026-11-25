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
	"testing"
	"time"
)

var errMetricsAccept = errors.New("permanent metrics accept failure")

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
			bind := func() net.Listener {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = listener.Close() })
				return listener
			}
			wait := func(done <-chan struct{}, event string) {
				t.Helper()
				select {
				case <-done:
				case <-time.After(timeout):
					t.Fatalf("timed out waiting for %s", event)
				}
			}
			checkHTTP := func(listener net.Listener, path string) {
				t.Helper()
				client := &http.Client{Timeout: timeout, Transport: &http.Transport{DisableKeepAlives: true}}
				resp, err := client.Get("http://" + listener.Addr().String() + path)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("%s status = %d, want 200", path, resp.StatusCode)
				}
			}
			ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

			public := bind()
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			srv := newServer(ok, logger)

			failMetrics := make(chan struct{})
			var metricsListener net.Listener
			var failedMetrics *failingMetricsListener
			var metricsSrv *http.Server
			if tt.metricsEnabled {
				failedMetrics = &failingMetricsListener{Listener: bind(), fail: failMetrics, closed: make(chan struct{})}
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

			checkHTTP(public, "/healthz")
			if tt.metricsEnabled {
				checkHTTP(metricsListener, "/metrics")
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
			checkHTTP(public, "/healthz")
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
			}
			if conn, err := net.DialTimeout("tcp", public.Addr().String(), timeout); err == nil {
				_ = conn.Close()
				t.Fatal("public listener still accepts connections after shutdown")
			}
		})
	}
}
