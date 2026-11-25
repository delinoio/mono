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
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/accounts-api/internal/metrics"
)

// The tests in this file cover the signal wiring of the real process. Signal
// registration is process-global, so they re-execute the test binary as a helper
// process that runs the production shutdownContext, newServer and runServers, and
// signal that process from the parent.

const (
	// signalHelperEnv switches the test binary into helper-process mode.
	signalHelperEnv            = "ACCOUNTS_API_SIGNAL_HELPER"
	signalHelperHealthy        = "healthy"
	signalHelperMetricsFailure = "metrics-failure"
	signalHelperPublicFailure  = "public-failure"
	// helperDrainTimeout is the drain timeout of the helper. It is far above
	// every limit the parent test waits for, so an exit that waits for the drain
	// to time out is distinguishable from a prompt one.
	helperDrainTimeout = 30 * time.Second
)

var errPublicAccept = errors.New("public accept failed permanently")

// TestSignalHelperProcess is not a test. The signal tests re-execute the test
// binary into it. It serves /ping, /slow?ms=<n> (a handler that blocks for n
// milliseconds) and /hold (a handler that blocks until the parent releases it
// over the helper's stdin), prints "listening <addr>", "slow_started" and
// "hold_started" lines, and exits with runServers' result. In either listener
// failure mode, the first byte fails Accept and the third lets the worker finish.
func TestSignalHelperProcess(t *testing.T) {
	mode := os.Getenv(signalHelperEnv)
	if mode != signalHelperHealthy && mode != signalHelperMetricsFailure && mode != signalHelperPublicFailure {
		t.Skip("helper process for the signal tests")
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	listener := newTestListener(t)
	metricsListener := newTestListener(t)
	releaseRequest := make(chan struct{})
	worker := func(ctx context.Context) { <-ctx.Done() }
	timeout := helperDrainTimeout
	if mode != signalHelperHealthy {
		failListener := make(chan struct{})
		finishWorker := make(chan struct{})
		if mode == signalHelperMetricsFailure {
			metricsListener = gatedMetricsListener{Listener: metricsListener, fail: failListener}
		} else {
			publicListener := listener
			listener = acceptErrorOnCloseListener{Listener: publicListener, err: errPublicAccept}
			go func() {
				<-failListener
				_ = publicListener.Close()
			}()
		}
		timeout = shutdownTimeout
		worker = func(ctx context.Context) {
			<-ctx.Done()
			logger.Info("worker_draining")
			<-finishWorker
			logger.Info("worker_finished")
		}
		go releaseOnControlInput(failListener, releaseRequest, finishWorker)
	} else {
		go releaseOnControlInput(releaseRequest)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /ping", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, r *http.Request) {
		ms, _ := strconv.Atoi(r.URL.Query().Get("ms"))
		fmt.Println("slow_started")
		time.Sleep(time.Duration(ms) * time.Millisecond)
	})
	mux.HandleFunc("GET /hold", func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("hold_started")
		select {
		case <-releaseRequest:
		case <-r.Context().Done():
		}
		if err := r.Context().Err(); err != nil {
			logger.Info("held_request_cancelled", slog.Any("error", err))
			http.Error(w, "request cancelled", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "completed")
	})

	serveMetrics := func(ctx context.Context) error {
		return metrics.Serve(ctx, metricsListener, http.NotFoundHandler(), logger)
	}
	ctx, stop := shutdownContext()
	fmt.Println("listening", listener.Addr().String())
	err := runServers(ctx, stop, newServer(mux, logger), listener, logger, timeout, worker, serveMetrics)
	stop()
	if err != nil {
		logger.Error("server_exited", slog.Any("error", err))
		os.Exit(1)
	}
	os.Exit(0)
}

// releaseOnControlInput opens each gate after a byte from the parent. Closing
// the pipe during cleanup opens any remaining gates.
func releaseOnControlInput(gates ...chan struct{}) {
	var control [1]byte
	for _, gate := range gates {
		_, _ = io.ReadFull(os.Stdin, control[:])
		close(gate)
	}
}

type gatedMetricsListener struct {
	net.Listener
	fail <-chan struct{}
}

func (l gatedMetricsListener) Accept() (net.Conn, error) {
	<-l.fail
	return nil, errMetricsAccept
}

// helperProcess is a running helper process and the output it printed.
type helperProcess struct {
	t      *testing.T
	cmd    *exec.Cmd
	addr   string
	exited chan struct{}
	out    *syncBuffer
	// control is the write end of the helper's stdin. Each byte opens one gate.
	control io.WriteCloser
}

func startHelperProcess(t *testing.T, mode string) *helperProcess {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the helper process is signalled with POSIX signals")
	}
	out := &syncBuffer{}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSignalHelperProcess$")
	cmd.Env = append(os.Environ(), signalHelperEnv+"="+mode)
	cmd.Stdout = out
	cmd.Stderr = out
	control, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("create the helper control pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		_ = control.Close()
		t.Fatalf("start helper: %v", err)
	}
	h := &helperProcess{t: t, cmd: cmd, exited: make(chan struct{}), out: out, control: control}
	// Wait runs once, here: it reaps the process and flushes the output copy.
	go func() {
		_ = cmd.Wait()
		close(h.exited)
	}()
	t.Cleanup(func() {
		// Closing the control pipe releases a handler that is still held; the
		// kill and the reap then end the process whatever state it is in.
		_ = control.Close()
		_ = cmd.Process.Kill()
		<-h.exited
	})

	line := h.waitForLine("listening ")
	h.addr = strings.TrimSpace(line[strings.Index(line, "listening ")+len("listening "):])
	return h
}

// waitForLine returns the first output line that contains substr.
func (h *helperProcess) waitForLine(substr string) string {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, line := range strings.Split(h.out.String(), "\n") {
			if strings.Contains(line, substr) {
				return line
			}
		}
		select {
		case <-h.exited:
			// The output is complete once the process has exited.
			for _, line := range strings.Split(h.out.String(), "\n") {
				if strings.Contains(line, substr) {
					return line
				}
			}
			h.t.Fatalf("helper exited before printing a line containing %q; output:\n%s", substr, h.out.String())
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for a line containing %q; output so far:\n%s", substr, h.out.String())
		}
	}
}

func (h *helperProcess) signal(sig syscall.Signal) {
	h.t.Helper()
	if err := h.cmd.Process.Signal(sig); err != nil {
		h.t.Fatalf("signal %v: %v", sig, err)
	}
}

// wait waits for the helper to exit within limit and returns its wait status.
// It fails the test immediately when the helper is still running after limit,
// so a slow exit never reaches the caller; callers only see a real exit.
func (h *helperProcess) wait(limit time.Duration) syscall.WaitStatus {
	h.t.Helper()
	select {
	case <-h.exited:
	case <-time.After(limit):
		h.t.Fatalf("helper still running %v after the signal; output:\n%s", limit, h.out.String())
	}
	return h.cmd.ProcessState.Sys().(syscall.WaitStatus)
}

// release opens the next gate in the helper.
func (h *helperProcess) release() {
	h.t.Helper()
	if _, err := h.control.Write([]byte{1}); err != nil {
		h.t.Fatalf("write the release byte to the helper control pipe: %v; output:\n%s", err, h.out.String())
	}
}

// finished reports whether the helper has exited.
func (h *helperProcess) finished() bool {
	select {
	case <-h.exited:
		return true
	default:
		return false
	}
}

func (h *helperProcess) waitForListenerClosure() {
	h.t.Helper()
	// server_shutdown_started is logged just before the server closes its
	// listeners. A failed dial proves that the public listener is closed.
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", h.addr, time.Second)
		if err != nil {
			return
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			h.t.Fatalf("helper listener still accepts connections after shutdown started; output:\n%s", h.out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// getAsync starts a GET and delivers the status code, or -1 on a client error.
func getAsync(url string) <-chan int {
	return getAsyncContext(context.Background(), url)
}

// getAsyncContext is getAsync with a request that ctx can cancel.
func getAsyncContext(ctx context.Context, url string) <-chan int {
	status := make(chan int, 1)
	go func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			status <- -1
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			status <- -1
			return
		}
		_ = resp.Body.Close()
		status <- resp.StatusCode
	}()
	return status
}

func TestSecondTerminationSignalTerminatesProcessDuringDrain(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			h := startHelperProcess(t, signalHelperHealthy)
			// The request outlives the whole test, so only the second signal can
			// end the process before helperDrainTimeout.
			getAsync("http://" + h.addr + "/slow?ms=60000")
			h.waitForLine("slow_started")

			h.signal(sig)
			h.waitForLine("server_shutdown_started")
			h.signal(sig)

			status := h.wait(5 * time.Second)
			if !status.Signaled() || status.Signal() != sig {
				t.Errorf("wait status = %v, want terminated by %v", status, sig)
			}
			if strings.Contains(h.out.String(), "server_shutdown_completed") {
				t.Errorf("output reports a completed shutdown:\n%s", h.out.String())
			}
		})
	}
}

func TestSingleTerminationSignalDrainsInFlightRequestAndExitsZero(t *testing.T) {
	h := startHelperProcess(t, signalHelperHealthy)
	// The /hold handler returns only when the test releases it, so the request
	// is guaranteed to be in flight while the signal and the shutdown happen.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	status := getAsyncContext(ctx, "http://"+h.addr+"/hold")
	h.waitForLine("hold_started")
	requireHeld := func(stage string) {
		t.Helper()
		select {
		case got := <-status:
			t.Fatalf("held request finished with status %d %s; output:\n%s", got, stage, h.out.String())
		default:
		}
		if h.finished() {
			t.Fatalf("helper exited %s; output:\n%s", stage, h.out.String())
		}
	}
	requireHeld("before the signal")

	h.signal(syscall.SIGTERM)
	h.waitForLine("server_shutdown_started")
	h.waitForListenerClosure()
	requireHeld("while the shutdown waits for the held request")
	if strings.Contains(h.out.String(), "server_shutdown_completed") {
		t.Fatalf("shutdown completed while the request was still held; output:\n%s", h.out.String())
	}

	h.release()
	select {
	case got := <-status:
		if got != http.StatusOK {
			t.Errorf("in-flight request status = %d, want %d; output:\n%s", got, http.StatusOK, h.out.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("held request did not finish within 5s of the release; output:\n%s", h.out.String())
	}
	ws := h.wait(5 * time.Second)
	if !ws.Exited() || ws.ExitStatus() != 0 {
		t.Errorf("wait status = %v, want exit status 0", ws)
	}
	h.waitForLine("server_shutdown_completed")
}

func TestSingleTerminationSignalWithoutTrafficExitsZero(t *testing.T) {
	h := startHelperProcess(t, signalHelperHealthy)

	resp, err := http.Get("http://" + h.addr + "/ping")
	if err != nil {
		t.Fatalf("GET /ping before any signal: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /ping status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	h.signal(syscall.SIGTERM)
	h.waitForLine("server_shutdown_completed")
	ws := h.wait(5 * time.Second)
	if !ws.Exited() || ws.ExitStatus() != 0 {
		t.Errorf("wait status = %v, want exit status 0", ws)
	}
}

func TestFirstTerminationSignalPreservesMetricsFailureDrain(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			h := startHelperProcess(t, signalHelperMetricsFailure)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			status := getAsyncContext(ctx, "http://"+h.addr+"/hold")
			h.waitForLine("hold_started")

			h.release() // Fail the bound metrics listener's Accept call.
			h.waitForLine("server_shutdown_started")
			h.waitForListenerClosure()

			h.signal(sig)
			h.release() // Let the request finish.
			select {
			case got := <-status:
				if got != http.StatusOK {
					t.Errorf("in-flight request status = %d, want %d; output:\n%s", got, http.StatusOK, h.out.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("held request did not finish after release; output:\n%s", h.out.String())
			}

			h.waitForLine(`"msg":"worker_draining"`)
			h.release() // Let the worker finish.
			ws := h.wait(5 * time.Second)
			if !ws.Exited() || ws.ExitStatus() != 1 {
				t.Fatalf("wait status = %v, want exit status 1; output:\n%s", ws, h.out.String())
			}
			h.waitForLine(`"msg":"worker_finished"`)
			exitLog := h.waitForLine(`"msg":"server_exited"`)
			if !strings.Contains(exitLog, errMetricsAccept.Error()) {
				t.Fatalf("exit log lacks the original metrics error: %s", exitLog)
			}
		})
	}
}

func TestServePublicListenerFailureDrainsProcess(t *testing.T) {
	for _, tc := range []struct {
		name   string
		signal syscall.Signal
	}{
		{name: "without signal"},
		{name: "with SIGTERM", signal: syscall.SIGTERM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := startHelperProcess(t, signalHelperPublicFailure)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			status := getAsyncContext(ctx, "http://"+h.addr+"/hold")
			h.waitForLine("hold_started")

			h.release() // Close the public socket and inject a permanent Accept error.
			h.waitForLine("server_shutdown_started")
			h.waitForListenerClosure()
			if tc.signal != 0 {
				h.signal(tc.signal)
			}
			h.release() // Let the held request finish with a live context.
			select {
			case got := <-status:
				if got != http.StatusOK {
					t.Fatalf("in-flight response status = %d, want 200; output:\n%s", got, h.out.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("held request did not finish after release; output:\n%s", h.out.String())
			}

			h.waitForLine(`"msg":"worker_draining"`)
			h.release()
			ws := h.wait(5 * time.Second)
			if !ws.Exited() || ws.ExitStatus() != 1 {
				t.Fatalf("wait status = %v, want exit status 1; output:\n%s", ws, h.out.String())
			}
			h.waitForLine(`"msg":"worker_finished"`)
			exitLog := h.waitForLine(`"msg":"server_exited"`)
			if !strings.Contains(exitLog, errPublicAccept.Error()) {
				t.Fatalf("exit log lacks the original public listener error: %s", exitLog)
			}
		})
	}
}

func TestAwaitServeCallsStopBeforeLoggingShutdownStarted(t *testing.T) {
	logs := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// serveErr only becomes ready inside stop. That keeps the select in
	// awaitServe from racing between a ready ctx and a ready serveErr, and it
	// models Serve returning once the drain has started.
	serveErr := make(chan error, 1)
	var logsAtStop string
	stopCalls := 0
	stop := func() {
		stopCalls++
		logsAtStop = logs.String()
		serveErr <- http.ErrServerClosed
	}
	if err := awaitServe(ctx, stop, &http.Server{}, serveErr, logger, time.Second); err != nil {
		t.Fatalf("awaitServe = %v, want nil", err)
	}
	if stopCalls != 1 {
		t.Errorf("stop called %d times, want 1", stopCalls)
	}
	if strings.Contains(logsAtStop, "server_shutdown_started") {
		t.Errorf("stop ran after server_shutdown_started was logged:\n%s", logsAtStop)
	}
	if !strings.Contains(logs.String(), "server_shutdown_started") {
		t.Errorf("logs lack server_shutdown_started:\n%s", logs.String())
	}
}

func TestAwaitServeDoesNotCallStopWhenServeFailsFirst(t *testing.T) {
	serveErr := make(chan error, 1)
	serveErr <- fmt.Errorf("accept failed")
	stopCalled := false
	err := awaitServe(context.Background(), func() { stopCalled = true }, &http.Server{}, serveErr, newTestLogger(), time.Second)
	if err == nil {
		t.Fatal("awaitServe = nil, want the serve error")
	}
	if stopCalled {
		t.Error("stop called on the serve-error path, want it left to the caller's defer")
	}
}
