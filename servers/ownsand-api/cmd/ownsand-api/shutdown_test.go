// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// acceptFailureListener accepts the first connection normally. The next Accept
// waits for a test to inject a permanent error or for the server to close.
type acceptFailureListener struct {
	net.Listener
	accepted bool
	fail     chan struct{}
	closed   chan struct{}
	err      error
}

func (l *acceptFailureListener) Accept() (net.Conn, error) {
	if !l.accepted {
		l.accepted = true
		return l.Listener.Accept()
	}
	select {
	case <-l.fail:
		return nil, l.err
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *acceptFailureListener) Close() error {
	defer close(l.closed)
	return l.Listener.Close()
}

type closeErrorListener struct {
	net.Listener
	err error
}

func (l closeErrorListener) Close() error {
	_ = l.Listener.Close()
	return l.err
}

type shutdownTemporaryAcceptError struct{ error }

func (shutdownTemporaryAcceptError) Timeout() bool   { return false }
func (shutdownTemporaryAcceptError) Temporary() bool { return true }

// acceptOnShutdownListener serves one connection, then holds the next Accept
// result until Shutdown sets its flag and closes the listener.
type acceptOnShutdownListener struct {
	net.Listener
	temporaryErr error
	acceptErr    error
	closeErr     error
	accepted     bool
	waiting      chan struct{}
	closed       chan struct{}
}

func (l *acceptOnShutdownListener) Accept() (net.Conn, error) {
	if l.temporaryErr != nil {
		err := l.temporaryErr
		l.temporaryErr = nil
		return nil, err
	}
	if !l.accepted {
		l.accepted = true
		return l.Listener.Accept()
	}
	close(l.waiting)
	<-l.closed
	return nil, l.acceptErr
}

func (l *acceptOnShutdownListener) Close() error {
	defer close(l.closed)
	return errors.Join(l.Listener.Close(), l.closeErr)
}

func shutdownTestListener(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func waitForShutdownTest[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for shutdown test event")
		var zero T
		return zero
	}
}

type shutdownResponse struct {
	body string
	err  error
}

func fetchForShutdownTest(t *testing.T, client *http.Client, l net.Listener, path string) <-chan shutdownResponse {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+l.Addr().String()+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan shutdownResponse, 1)
	go func() {
		resp, err := client.Do(req)
		if err != nil {
			done <- shutdownResponse{err: err}
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			done <- shutdownResponse{err: fmt.Errorf("HTTP status %d", resp.StatusCode)}
			return
		}
		body, err := io.ReadAll(resp.Body)
		done <- shutdownResponse{body: string(body), err: err}
	}()
	return done
}

func requireShutdownResponse(t *testing.T, done <-chan shutdownResponse, want string) {
	t.Helper()
	if got := waitForShutdownTest(t, done); got.err != nil || got.body != want {
		t.Fatalf("response = %q, %v, want %q", got.body, got.err, want)
	}
}

func requireShutdownPending(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("server returned with a request still open: %v", err)
	default:
	}
}

func TestServeDrainsAfterPublicListenerFailure(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cancelFirst bool
		forceClose  bool
		cleanupErr  error
	}{
		{name: "graceful drain"},
		{name: "forced close", forceClose: true},
		{name: "cancellation first", cancelFirst: true},
		{name: "cleanup failure", cleanupErr: errors.New("close other public listener")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener := &acceptFailureListener{
				Listener: shutdownTestListener(t), fail: make(chan struct{}),
				closed: make(chan struct{}), err: errors.New("permanent public accept error"),
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			requestStarted := make(chan struct{})
			requestCanceled := make(chan struct{})
			releaseRequest := make(chan struct{})
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			srv := newServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(requestStarted)
				select {
				case <-releaseRequest:
					_, _ = io.WriteString(w, r.Proto)
				case <-r.Context().Done():
					close(requestCanceled)
				}
			}), logger)
			if tc.cleanupErr != nil {
				// The failed listener is already closed by Serve. A second
				// listener can still report a close error during Shutdown.
				other := closeErrorListener{shutdownTestListener(t), tc.cleanupErr}
				otherStarted := make(chan struct{})
				srv.BaseContext = func(l net.Listener) context.Context {
					if l == other {
						close(otherStarted)
					}
					return context.Background()
				}
				otherDone := make(chan error, 1)
				go func() { otherDone <- srv.Serve(other) }()
				waitForShutdownTest(t, otherStarted)
				t.Cleanup(func() {
					_ = srv.Close()
					if err := waitForShutdownTest(t, otherDone); !errors.Is(err, http.ErrServerClosed) {
						t.Errorf("other Serve error = %v, want http.ErrServerClosed", err)
					}
				})
			}
			var protocols http.Protocols
			protocols.SetUnencryptedHTTP2(true)
			client := &http.Client{Transport: &http.Transport{Protocols: &protocols}}
			t.Cleanup(client.CloseIdleConnections)

			timeout := 3 * time.Second
			if tc.forceClose {
				timeout = 100 * time.Millisecond
			}
			workersStopped := make(chan struct{})
			allowDrain := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				done <- serve(ctx, func() {}, srv, listener, logger, timeout, func() {
					close(workersStopped)
					select {
					case <-allowDrain:
					case <-t.Context().Done():
					}
				})
			}()
			resp := fetchForShutdownTest(t, client, listener, "/")
			waitForShutdownTest(t, requestStarted)
			if tc.cancelFirst {
				cancel()
				waitForShutdownTest(t, workersStopped)
			}
			close(listener.fail)
			waitForShutdownTest(t, listener.closed)
			waitForShutdownTest(t, workersStopped)
			requireShutdownPending(t, done)
			if tc.forceClose {
				select {
				case <-requestCanceled:
					t.Fatal("request canceled before the drain could start")
				default:
				}
				start := time.Now()
				close(allowDrain)
				waitForShutdownTest(t, requestCanceled)
				if time.Since(start) < timeout {
					t.Fatal("request canceled before the HTTP drain deadline")
				}
				if got := waitForShutdownTest(t, resp); got.err == nil {
					t.Fatal("request succeeded after the forced close")
				}
			} else {
				close(allowDrain)
				close(releaseRequest)
				requireShutdownResponse(t, resp, "HTTP/2.0")
			}
			err := waitForShutdownTest(t, done)
			if !errors.Is(err, listener.err) || (tc.cleanupErr != nil && !errors.Is(err, tc.cleanupErr)) {
				t.Fatalf("serve = %v, want listener error %v and cleanup error %v", err, listener.err, tc.cleanupErr)
			}
		})
	}
}

func TestServerShutdownRetainsPublicAcceptFailure(t *testing.T) {
	acceptErr := errors.New("permanent public accept error")
	closeErr := errors.New("public listener cleanup failed")
	flushErr := errors.New("final usage flush failed")
	closed := fmt.Errorf("public listener: %w", net.ErrClosed)
	for _, tc := range []struct {
		name         string
		acceptErr    error
		temporaryErr error
		closeErr     error
		flushErr     error
		wantErrors   []error
	}{
		{name: "permanent failure", acceptErr: acceptErr, wantErrors: []error{acceptErr}},
		{name: "cleanup and usage failure", acceptErr: acceptErr, closeErr: closeErr, flushErr: flushErr, wantErrors: []error{acceptErr, closeErr, flushErr}},
		{name: "healthy cancellation", acceptErr: closed},
		{name: "temporary failure before healthy cancellation", temporaryErr: shutdownTemporaryAcceptError{errors.New("retry public accept")}, acceptErr: closed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener := &acceptOnShutdownListener{
				Listener: shutdownTestListener(t), temporaryErr: tc.temporaryErr, acceptErr: tc.acceptErr, closeErr: tc.closeErr,
				waiting: make(chan struct{}), closed: make(chan struct{}),
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			requestStarted := make(chan struct{})
			releaseRequest := make(chan struct{})
			requestFinished := make(chan struct{})
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			srv := newServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(requestStarted)
				defer close(requestFinished)
				select {
				case <-releaseRequest:
					_, _ = io.WriteString(w, "finished")
				case <-r.Context().Done():
				}
			}), logger)
			usage := &fakeUsage{flushErr: tc.flushErr, drain: func(context.Context) error {
				select {
				case <-requestFinished:
				default:
					t.Error("usage drain started while the public request was still open")
				}
				return nil
			}}

			done := make(chan error, 1)
			go func() {
				done <- runServers(ctx, func() {}, srv, listener, logger, 3*time.Second, func() {}, usage, nil, nil)
			}()
			client := &http.Client{Transport: &http.Transport{}}
			t.Cleanup(client.CloseIdleConnections)
			resp := fetchForShutdownTest(t, client, listener, "/hold")
			waitForShutdownTest(t, requestStarted)
			waitForShutdownTest(t, listener.waiting)
			cancel()
			waitForShutdownTest(t, listener.closed)
			requireShutdownPending(t, done)
			close(releaseRequest)
			requireShutdownResponse(t, resp, "finished")

			err := waitForShutdownTest(t, done)
			if !slices.Equal(usage.calls, []string{"drain", "flush"}) {
				t.Errorf("usage calls = %v, want drain then flush", usage.calls)
			}
			if len(tc.wantErrors) == 0 {
				if err != nil {
					t.Fatalf("clean shutdown: %v", err)
				}
				return
			}

			var logs bytes.Buffer
			logServerExited(slog.New(slog.NewJSONHandler(&logs, nil)), err)
			var record struct {
				Message string `json:"msg"`
				Error   string `json:"error"`
			}
			if jsonErr := json.Unmarshal(logs.Bytes(), &record); jsonErr != nil || record.Message != "server_exited" {
				t.Fatalf("server_exited record = %s, decode error = %v", logs.Bytes(), jsonErr)
			}
			for _, want := range tc.wantErrors {
				if !errors.Is(err, want) {
					t.Errorf("runServers = %v, want cause %v", err, want)
				}
				if strings.Count(record.Error, want.Error()) != 1 {
					t.Errorf("server_exited error = %q, want cause %q exactly once", record.Error, want)
				}
			}
		})
	}
}

func TestServerShutdownAfterMetricsFailureOrSignal(t *testing.T) {
	type shutdownOrder int
	const (
		metricsOnly shutdownOrder = iota
		metricsFirst
		signalFirst
		signalOnly
	)
	for _, tc := range []struct {
		name           string
		order          shutdownOrder
		metricsEnabled bool
	}{
		{name: "metrics failure alone", order: metricsOnly, metricsEnabled: true},
		{name: "metrics then signal", order: metricsFirst, metricsEnabled: true},
		{name: "signal then metrics", order: signalFirst, metricsEnabled: true},
		{name: "signal with metrics", order: signalOnly, metricsEnabled: true},
		{name: "signal without metrics", order: signalOnly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			stopSignalCalled := make(chan struct{})
			workersStopped := make(chan struct{})
			publicDraining := make(chan struct{})
			requestStarted := make(chan struct{})
			releaseRequest := make(chan struct{})
			requestFinished := make(chan struct{})
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			public := shutdownTestListener(t)
			srv := newServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/healthz" {
					_, _ = io.WriteString(w, "ok")
					return
				}
				close(requestStarted)
				defer close(requestFinished)
				select {
				case <-releaseRequest:
					_, _ = io.WriteString(w, "finished")
				case <-r.Context().Done():
				}
			}), logger)
			srv.RegisterOnShutdown(func() { close(publicDraining) })

			var metricsListener net.Listener
			var metricsSrv *http.Server
			var failingMetrics *acceptFailureListener
			if tc.metricsEnabled {
				failingMetrics = &acceptFailureListener{
					Listener: shutdownTestListener(t), fail: make(chan struct{}),
					closed: make(chan struct{}), err: errors.New("permanent metrics accept error"),
				}
				metricsListener = failingMetrics
				metricsSrv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.WriteString(w, "metric 1\n")
				})}
			}
			usage := &fakeUsage{drain: func(context.Context) error {
				select {
				case <-requestFinished:
				default:
					t.Error("usage drain started while a public request was still open")
				}
				if tc.metricsEnabled && tc.order == signalOnly {
					select {
					case <-failingMetrics.closed:
						t.Error("healthy metrics listener closed before usage drain")
					default:
					}
				}
				return nil
			}}
			if tc.order == metricsOnly {
				usage.flushErr = errors.New("final usage flush failed")
			}

			done := make(chan error, 1)
			go func() {
				done <- runServers(ctx, func() { close(stopSignalCalled) }, srv, public, logger, 3*time.Second,
					func() { close(workersStopped) }, usage, metricsSrv, metricsListener)
			}()
			client := &http.Client{Transport: &http.Transport{}}
			t.Cleanup(client.CloseIdleConnections)
			requireShutdownResponse(t, fetchForShutdownTest(t, client, public, "/healthz"), "ok")
			if tc.metricsEnabled {
				requireShutdownResponse(t, fetchForShutdownTest(t, client, metricsListener, "/metrics"), "metric 1\n")
			}
			resp := fetchForShutdownTest(t, client, public, "/hold")
			waitForShutdownTest(t, requestStarted)
			switch tc.order {
			case metricsOnly, metricsFirst:
				close(failingMetrics.fail)
			case signalFirst, signalOnly:
				cancel()
			}
			waitForShutdownTest(t, publicDraining)
			select {
			case <-workersStopped:
			default:
				t.Fatal("public drain started before workers stopped")
			}
			switch tc.order {
			case metricsFirst:
				select {
				case <-stopSignalCalled:
					t.Fatal("metrics failure removed the signal handler before a signal")
				default:
				}
				cancel()
			case signalFirst:
				close(failingMetrics.fail)
				waitForShutdownTest(t, failingMetrics.closed)
			}
			requireShutdownPending(t, done)
			close(releaseRequest)
			requireShutdownResponse(t, resp, "finished")
			err := waitForShutdownTest(t, done)
			if tc.order == signalOnly {
				if err != nil {
					t.Fatalf("clean shutdown: %v", err)
				}
			} else if !errors.Is(err, failingMetrics.err) || (usage.flushErr != nil && !errors.Is(err, usage.flushErr)) {
				t.Fatalf("runServers = %v, want metrics error %v and usage error %v", err, failingMetrics.err, usage.flushErr)
			}
			if !slices.Equal(usage.calls, []string{"drain", "flush"}) {
				t.Errorf("usage calls = %v, want drain then flush", usage.calls)
			}
			if tc.metricsEnabled {
				waitForShutdownTest(t, failingMetrics.closed)
			}
		})
	}
}
