// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/accounts-api/internal/metrics"
)

// errMetricsAccept is the permanent listener error that the tests inject. It
// does not implement net.Error with Temporary, so net/http does not retry it.
var errMetricsAccept = errors.New("metrics accept failed permanently")

// backgroundRig is a runServers call with a live public server, a billing
// worker stand-in and a metrics listener under the control of the test.
type backgroundRig struct {
	cancel        context.CancelFunc
	publicAddr    string
	logs          *syncBuffer
	workerStopped atomic.Bool
	workerStarted chan struct{}
	result        chan error
}

// startBackgroundRig starts runServers. serveMetrics may be nil, which
// disables the metrics listener like an empty METRICS_ADDR. The worker stand-in
// finishes its "in-progress work" for workerDrain after its context is
// cancelled, so a runServers that returns early is observable.
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
	const workerDrain = 150 * time.Millisecond
	worker := func(background context.Context) {
		close(rig.workerStarted)
		<-background.Done()
		time.Sleep(workerDrain)
		rig.workerStopped.Store(true)
	}
	srv := newServer(http.NewServeMux(), logger)
	go func() {
		rig.result <- runServers(ctx, noStop, srv, listener, logger, 5*time.Second, worker, serveMetrics)
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

// publicRefusesConnections reports whether the public listener is closed.
func (r *backgroundRig) publicRefusesConnections() bool {
	conn, err := net.DialTimeout("tcp", r.publicAddr, time.Second)
	if err == nil {
		_ = conn.Close()
		return false
	}
	return true
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
// without any signal or public listener failure, waits for the billing worker,
// and reports the original error.
func TestRunServersShutsDownOnTerminalMetricsListenerError(t *testing.T) {
	rig := startBackgroundRig(t, permanentAcceptMetrics(t))

	err := rig.await(t)
	if !errors.Is(err, errMetricsAccept) {
		t.Fatalf("runServers error = %v; want the metrics Accept error", err)
	}
	if !rig.workerStopped.Load() {
		t.Error("runServers returned before the billing worker finished its in-progress work")
	}
	if !rig.publicRefusesConnections() {
		t.Error("public listener still accepts connections after runServers returned")
	}
	logs := rig.logs.String()
	for _, want := range []string{"metrics_listener_failed", "server_shutdown_started", "server_shutdown_completed", errMetricsAccept.Error()} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %q; logs: %s", want, logs)
		}
	}
}

// TestRunServersKeepsMetricsErrorWhenShutdownSignalRaces verifies that the
// metrics listener error is not lost when the shutdown trigger fires first,
// or at the same time, and runServers has already started the drain.
func TestRunServersKeepsMetricsErrorWhenShutdownSignalRaces(t *testing.T) {
	for i := 0; i < 20; i++ {
		release := make(chan struct{})
		rig := startBackgroundRig(t, func(context.Context) error {
			<-release
			return errMetricsAccept
		})
		// The signal arrives, the drain starts and the metrics listener fails
		// in the same moment. Which of them runServers observes first varies.
		rig.cancel()
		close(release)

		err := rig.await(t)
		if !errors.Is(err, errMetricsAccept) {
			t.Fatalf("iteration %d: runServers error = %v; want the metrics Accept error", i, err)
		}
		if !rig.workerStopped.Load() {
			t.Fatalf("iteration %d: runServers returned before the billing worker finished", i)
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
	if !rig.workerStopped.Load() {
		t.Error("runServers returned before the billing worker finished its in-progress work")
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
	if !rig.workerStopped.Load() {
		t.Error("runServers returned before the billing worker finished its in-progress work")
	}
}
