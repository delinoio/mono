// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
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
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/delinoio/mono/servers/accounts-api/internal/metrics"
)

// errMetricsAccept is the permanent listener error that the tests inject. It
// does not implement net.Error with Temporary, so net/http does not retry it.
var errMetricsAccept = errors.New("metrics accept failed permanently")

func TestRunServersSkipsPreCanceledStartup(t *testing.T) {
	for _, tt := range []struct {
		name        string
		withMetrics bool
	}{
		{name: "with metrics", withMetrics: true},
		{name: "without metrics"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			logs := &syncBuffer{}
			logger := slog.New(slog.NewJSONHandler(logs, nil))
			listener := newTestListener(t)
			srv := newServer(http.NewServeMux(), logger)

			stopCalls := 0
			stop := func() { stopCalls++ }
			onStart := func() { logger.Info("server_started") }
			var workerCalls, metricsCalls atomic.Int32
			worker := func(context.Context) {
				workerCalls.Add(1)
				logger.Info("billing_worker_started")
			}
			var serveMetrics func(context.Context) error
			if tt.withMetrics {
				serveMetrics = func(context.Context) error {
					metricsCalls.Add(1)
					logger.Info("metrics_listener_started")
					return nil
				}
			}

			if err := runServers(ctx, stop, srv, listener, logger, time.Second, onStart, worker, runAsync, serveMetrics); err != nil {
				t.Fatalf("runServers error = %v, want nil", err)
			}
			if stopCalls != 1 || workerCalls.Load() != 0 || metricsCalls.Load() != 0 {
				t.Errorf("calls to stop, worker and metrics = %d, %d, %d, want 1, 0, 0",
					stopCalls, workerCalls.Load(), metricsCalls.Load())
			}
			conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
			if err == nil {
				_ = conn.Close()
				t.Error("public listener accepted a connection after canceled startup")
			}
			if output := logs.String(); output != "" {
				t.Errorf("logs after canceled startup = %q, want no logs", output)
			}
		})
	}
}

func TestRunServersClosesActivePublicConnectionAtDrainDeadline(t *testing.T) {
	for _, tc := range []struct {
		name         string
		acceptErr    error
		cancelBefore bool
	}{
		{name: "shutdown signal"},
		{name: "public listener failure", acceptErr: errPublicAccept},
		{name: "signal before public listener failure returns", acceptErr: errPublicAccept, cancelBefore: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			publicListener := newTestListener(t)
			listener := publicListener
			acceptFailed := make(chan struct{})
			if tc.cancelBefore {
				listener = newSignalAcceptListener(publicListener, tc.acceptErr, func() { close(acceptFailed) })
			} else if tc.acceptErr != nil {
				listener = acceptErrorOnCloseListener{Listener: publicListener, err: tc.acceptErr}
			}
			requestStarted := make(chan struct{})
			requestCancelled := make(chan struct{})
			releaseHandler := make(chan struct{})
			defer close(releaseHandler)
			logger := newTestLogger()
			srv := newServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				close(requestStarted)
				<-r.Context().Done()
				close(requestCancelled)
				<-releaseHandler
			}), logger)

			workerDraining := make(chan struct{})
			releaseWorker := make(chan struct{})
			workerFinished := make(chan struct{})
			worker := func(ctx context.Context) {
				<-ctx.Done()
				close(workerDraining)
				select {
				case <-releaseWorker:
				case <-t.Context().Done():
				}
				close(workerFinished)
			}
			const drainTimeout = 200 * time.Millisecond
			result := runAsync(func() error {
				return runServers(ctx, noStop, srv, listener, logger, drainTimeout, nil, worker, runAsync, nil)
			})

			conn, err := net.DialTimeout("tcp", publicListener.Addr().String(), handlerStartTimeout)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if _, err := fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			if err := awaitHandlerStarted(requestStarted, nil, result, handlerStartTimeout); err != nil {
				t.Fatal(err)
			}

			if tc.cancelBefore {
				select {
				case <-acceptFailed:
				case <-time.After(handlerStartTimeout):
					t.Fatal("listener did not select the permanent Accept error")
				}
			}
			if tc.acceptErr == nil || tc.cancelBefore {
				cancel()
			} else if err := publicListener.Close(); err != nil {
				t.Fatalf("fail public listener: %v", err)
			}
			select {
			case <-workerDraining:
			case <-time.After(handlerStartTimeout):
				t.Fatal("worker did not start draining after the public deadline")
			}
			select {
			case <-requestCancelled:
			case <-time.After(handlerStartTimeout):
				t.Fatal("request context remained live after the public deadline")
			}
			// Both gates are still closed, so neither a handler response nor
			// process shutdown can account for the connection closing.
			if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatal(err)
			}
			var data [1]byte
			if n, err := conn.Read(data[:]); n != 0 || (!errors.Is(err, io.EOF) && !errors.Is(err, syscall.ECONNRESET)) {
				t.Fatalf("read %d bytes with error %v, want EOF or connection reset", n, err)
			}
			select {
			case err := <-result:
				t.Fatalf("runServers returned %v while the worker was still draining", err)
			default:
			}
			close(releaseWorker)
			select {
			case err := <-result:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("runServers error = %v, want the drain deadline error", err)
				}
				if tc.acceptErr != nil && !errors.Is(err, tc.acceptErr) {
					t.Errorf("runServers error = %v, want the public listener error", err)
				}
			case <-time.After(handlerStartTimeout):
				t.Fatal("runServers did not return after the worker finished")
			}
			select {
			case <-workerFinished:
			default:
				t.Fatal("runServers returned before the worker finished")
			}
		})
	}
}

type channelListener struct {
	closed    chan struct{}
	closeOnce sync.Once
}

func (l *channelListener) Accept() (net.Conn, error) {
	<-l.closed
	return nil, net.ErrClosed
}

func (l *channelListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *channelListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}
}

func TestRunServersWaitsForBillingWorker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		listener := &channelListener{closed: make(chan struct{})}
		workerStarted := make(chan struct{})
		workerCanceled := make(chan struct{})
		workerRelease := make(chan struct{})
		releaseWorker := sync.OnceFunc(func() { close(workerRelease) })
		defer releaseWorker()

		worker := func(ctx context.Context) {
			close(workerStarted)
			<-ctx.Done()
			close(workerCanceled)
			<-workerRelease
		}
		logger := newTestLogger()
		srv := newServer(http.NewServeMux(), logger)
		result := runAsync(func() error {
			return runServers(ctx, noStop, srv, listener, logger, time.Second, nil, worker, runAsync, nil)
		})

		<-workerStarted
		synctest.Wait() // Let the public server reach Accept before shutdown.
		cancel()
		<-workerCanceled
		synctest.Wait()
		select {
		case err := <-result:
			t.Fatalf("runServers returned %v while the billing worker was still held", err)
		default:
		}

		releaseWorker()
		if err := <-result; err != nil {
			t.Fatalf("runServers error = %v, want nil after the billing worker finished", err)
		}
	})
}

// backgroundRig is a runServers call with a live public server, a billing
// worker stand-in and a metrics listener under the control of the test.
type backgroundRig struct {
	cancel        context.CancelFunc
	publicAddr    string
	logs          *syncBuffer
	workerStarted chan struct{}
	result        chan error
}

// startBackgroundRig starts runServers. serveMetrics may be nil, which
// disables the metrics listener like an empty METRICS_ADDR.
func startBackgroundRig(t *testing.T, serveMetrics func(context.Context) error) *backgroundRig {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	logs := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	listener := newTestListener(t)
	rig := &backgroundRig{
		cancel:        cancel,
		publicAddr:    listener.Addr().String(),
		logs:          logs,
		workerStarted: make(chan struct{}),
		result:        make(chan error, 1),
	}
	worker := func(background context.Context) {
		close(rig.workerStarted)
		<-background.Done()
	}
	srv := newServer(http.NewServeMux(), logger)
	go func() {
		rig.result <- runServers(ctx, noStop, srv, listener, logger, 5*time.Second, nil, worker, runAsync, serveMetrics)
	}()
	select {
	case <-rig.workerStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("billing worker stand-in did not start")
	}
	return rig
}

// await returns the runServers result or fails the test after a deadline.
func (r *backgroundRig) await(t *testing.T) error {
	t.Helper()
	select {
	case err := <-r.result:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("runServers did not return")
		return nil
	}
}

// permanentAcceptMetrics serves the metrics handler on a listener whose Accept
// fails with errMetricsAccept, after the bind has succeeded.
func permanentAcceptMetrics(t *testing.T) func(context.Context) error {
	t.Helper()
	bound := failingListener{Listener: newTestListener(t), err: errMetricsAccept}
	return func(ctx context.Context) error {
		return metrics.Serve(ctx, bound, http.NotFoundHandler(), newTestLogger())
	}
}

// TestRunServersShutsDownOnTerminalMetricsListenerError verifies that a
// metrics listener that fails after a successful bind stops the public server
// without any signal or public listener failure and reports the original error.
func TestRunServersShutsDownOnTerminalMetricsListenerError(t *testing.T) {
	rig := startBackgroundRig(t, permanentAcceptMetrics(t))

	err := rig.await(t)
	if !errors.Is(err, errMetricsAccept) {
		t.Fatalf("runServers error = %v; want the metrics Accept error", err)
	}
	if !rig.workerStopped.Load() {
		t.Error("runServers returned before the billing worker finished its in-progress work")
	}
	dialer := net.Dialer{Timeout: listenerProbeTimeout}
	refused, err := probeTCPRefusal(rig.publicAddr, dialer.Dial)
	if err != nil {
		t.Errorf("after runServers returned: %v", err)
	} else if !refused {
		t.Errorf("public listener at %s still accepts connections after runServers returned", rig.publicAddr)
	}
	logs := rig.logs.String()
	for _, want := range []string{"metrics_listener_failed", "server_shutdown_started", "server_shutdown_completed", errMetricsAccept.Error()} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %q; logs: %s", want, logs)
		}
	}
}

func newMetricsErrorGate(t *testing.T) (<-chan struct{}, func()) {
	t.Helper()
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	t.Cleanup(release)
	return gate, release
}

func TestRunServersKeepsMetricsErrorAfterFixtureCleanup(t *testing.T) {
	var rig *backgroundRig
	if !t.Run("fixture", func(fixture *testing.T) {
		gate, release := newMetricsErrorGate(fixture)
		// Release the gate from the parent if the fixture cleanup fails to do so.
		t.Cleanup(release)
		atGate := make(chan struct{})
		rig = startBackgroundRig(fixture, func(context.Context) error {
			close(atGate)
			<-gate
			return errMetricsAccept
		})
		select {
		case <-atGate:
		case <-time.After(handlerStartTimeout):
			fixture.Fatal("metrics callback did not reach the release gate")
		}
	}) || rig == nil {
		return
	}

	if err := rig.await(t); !errors.Is(err, errMetricsAccept) {
		t.Fatalf("runServers error = %v, want the metrics Accept error", err)
	}
	if !rig.workerStopped.Load() {
		t.Fatal("runServers returned before the billing worker finished")
	}
}

// TestRunServersKeepsMetricsErrorWhenShutdownSignalRaces verifies that the
// metrics listener error is not lost when the shutdown trigger fires first,
// or at the same time, and runServers has already started the drain.
func TestRunServersKeepsMetricsErrorWhenShutdownSignalRaces(t *testing.T) {
	for i := 0; i < 20; i++ {
		gate, release := newMetricsErrorGate(t)
		rig := startBackgroundRig(t, func(context.Context) error {
			<-gate
			return errMetricsAccept
		})
		// The signal arrives, the drain starts and the metrics listener fails
		// in the same moment. Which of them runServers observes first varies.
		rig.cancel()
		release()

		err := rig.await(t)
		if !errors.Is(err, errMetricsAccept) {
			t.Fatalf("iteration %d: runServers error = %v; want the metrics Accept error", i, err)
		}
	}
}

// TestRunServersCleanShutdownWithMetricsReturnsNil verifies that a signal
// shutdown with a healthy metrics listener is not an error: the metrics
// listener's http.ErrServerClosed outcome is filtered.
func TestRunServersCleanShutdownWithMetricsReturnsNil(t *testing.T) {
	listener := newTestListener(t)
	rig := startBackgroundRig(t, func(ctx context.Context) error {
		return metrics.Serve(ctx, listener, http.NotFoundHandler(), newTestLogger())
	})

	rig.cancel()
	if err := rig.await(t); err != nil {
		t.Fatalf("runServers error = %v; want nil after a clean shutdown", err)
	}
	if !strings.Contains(rig.logs.String(), "server_shutdown_completed") {
		t.Errorf("logs lack server_shutdown_completed; logs: %s", rig.logs.String())
	}
}

// TestRunServersWithoutMetricsListener verifies that a nil serveMetrics, which
// is what an empty METRICS_ADDR produces, starts no metrics task and still
// shuts down cleanly.
func TestRunServersWithoutMetricsListener(t *testing.T) {
	rig := startBackgroundRig(t, nil)

	rig.cancel()
	if err := rig.await(t); err != nil {
		t.Fatalf("runServers error = %v; want nil", err)
	}
}
