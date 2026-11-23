// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"context"
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
)

// The tests in this file cover the signal wiring of the real process. Signal
// registration is process-global, so they re-execute the test binary as a helper
// process that runs the production shutdownContext, newServer and serve, and
// signal that process from the parent.

const (
	// signalHelperEnv switches the test binary into helper-process mode.
	signalHelperEnv = "ACCOUNTS_API_SIGNAL_HELPER"
	// helperDrainTimeout is the drain timeout of the helper. It is far above
	// every limit the parent test waits for, so an exit that waits for the drain
	// to time out is distinguishable from a prompt one.
	helperDrainTimeout = 30 * time.Second
)

// TestSignalHelperProcess is not a test. The signal tests re-execute the test
// binary into it. It serves /ping, /slow?ms=<n> (a handler that blocks for n
// milliseconds) and /hold (a handler that blocks until the parent releases it
// over the helper's stdin), prints "listening <addr>", "slow_started" and
// "hold_started" lines, and exits with serve's result.
func TestSignalHelperProcess(t *testing.T) {
	if os.Getenv(signalHelperEnv) != "1" {
		t.Skip("helper process for the signal tests")
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ping", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, r *http.Request) {
		ms, _ := strconv.Atoi(r.URL.Query().Get("ms"))
		fmt.Println("slow_started")
		time.Sleep(time.Duration(ms) * time.Millisecond)
	})
	mux.HandleFunc("GET /hold", func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("hold_started")
		// The handler returns only when the parent writes the release byte, or
		// closes the control pipe (which is what the test cleanup does). It is
		// never time based, so the request is still in flight whenever the parent
		// has not released it, however long the parent is stalled.
		_, _ = io.ReadFull(os.Stdin, make([]byte, 1))
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	ctx, stop := shutdownContext()
	defer stop()
	fmt.Println("listening", listener.Addr().String())
	if err := serve(ctx, stop, newServer(mux, logger), listener, logger, helperDrainTimeout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

// helperProcess is a running helper process and the output it printed.
type helperProcess struct {
	t      *testing.T
	cmd    *exec.Cmd
	addr   string
	exited chan struct{}
	out    *syncBuffer
	// control is the write end of the helper's stdin. The /hold handler blocks
	// until it receives a byte on it.
	control io.WriteCloser
}

func startHelperProcess(t *testing.T) *helperProcess {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the helper process is signalled with POSIX signals")
	}
	out := &syncBuffer{}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSignalHelperProcess$")
	cmd.Env = append(os.Environ(), signalHelperEnv+"=1")
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

// release lets the /hold handler of the helper return.
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
			h := startHelperProcess(t)
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
	h := startHelperProcess(t)
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

	h.signal(syscall.SIGINT)
	h.waitForLine("server_shutdown_started")

	// server_shutdown_started is logged just before the server closes its
	// listeners, so the listener only stops accepting a moment later. A fresh
	// dial that fails proves that the shutdown has reached the listener.
	dialDeadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", h.addr, time.Second)
		if err != nil {
			break
		}
		_ = conn.Close()
		if time.Now().After(dialDeadline) {
			t.Fatalf("helper listener still accepts connections after the shutdown started; output:\n%s", h.out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
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
	h := startHelperProcess(t)

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
