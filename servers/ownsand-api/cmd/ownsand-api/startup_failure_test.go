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
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/internal/telemetry"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"
)

// The errno rules run on every host: the errno values and the platform name
// are injected, so the Windows rule is covered on Linux and the Unix rule on
// Windows.
func TestIsAddressInUseErrno(t *testing.T) {
	cases := []struct {
		name  string
		errno syscall.Errno
		goos  string
		want  bool
	}{
		{"unix EADDRINUSE", syscall.EADDRINUSE, "linux", true},
		{"darwin EADDRINUSE", syscall.EADDRINUSE, "darwin", true},
		{"unix permission", syscall.EACCES, "linux", false},
		{"windows native WSAEADDRINUSE", 10048, "windows", true},
		{"windows WSAEACCES", 10013, "windows", false},
		{"windows access denied", 5, "windows", false},
		// A Unix errno number must not be taken for the Windows error.
		{"windows with a Unix number", 98, "windows", false},
	}
	for _, tc := range cases {
		if got := isAddressInUseErrno(tc.errno, tc.goos); got != tc.want {
			t.Errorf("%s: isAddressInUseErrno(%d, %q) = %v, want %v", tc.name, uintptr(tc.errno), tc.goos, got, tc.want)
		}
	}
}

// bindError builds the error shape of a failed net.Listen: an OpError that
// wraps an os.SyscallError that wraps the errno.
func bindError(errno syscall.Errno) error {
	return &net.OpError{Op: "listen", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}, Err: os.NewSyscallError("bind", errno)}
}

func TestClassifyBindErrorForWrappedErrno(t *testing.T) {
	cases := []struct {
		name string
		err  error
		goos string
		want startupFailureReason
	}{
		{"wrapped EADDRINUSE", bindError(syscall.EADDRINUSE), "linux", startupFailureReasonAddressInUse},
		{"wrapped Winsock 10048", bindError(10048), "windows", startupFailureReasonAddressInUse},
		{"doubly wrapped", fmt.Errorf("listen: %w", bindError(syscall.EADDRINUSE)), "linux", startupFailureReasonAddressInUse},
		{"permission denied", bindError(syscall.EACCES), "linux", ""},
		{"Winsock access denied", bindError(10013), "windows", ""},
		{"not an errno", errors.New("address already in use"), "linux", ""},
		{"Windows text without the errno", errors.New("Only one usage of each socket address (protocol/network address/port) is normally permitted."), "windows", ""},
	}
	for _, tc := range cases {
		got := classifyBindErrorFor(tc.err, tc.goos)
		if reason := startupFailureReasonOf(got); reason != tc.want {
			t.Errorf("%s: reason = %q, want %q", tc.name, reason, tc.want)
		}
		// The original error stays reachable and unchanged in text.
		if !errors.Is(got, tc.err) || got.Error() != tc.err.Error() {
			t.Errorf("%s: the classified error does not keep the original error", tc.name)
		}
	}
	if classifyBindErrorFor(nil, "linux") != nil {
		t.Error("classifyBindErrorFor(nil) must be nil")
	}
}

// A real bind collision on the running platform produces the native errno
// (EADDRINUSE on Unix, Winsock 10048 on Windows) that the classifier must
// recognize, for HTTP and for STUN.
func TestClassifyRealBindCollisions(t *testing.T) {
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	_, err = net.Listen("tcp", tcp.Addr().String())
	if err == nil {
		t.Fatal("a second TCP listener on a held address succeeded")
	}
	if got := startupFailureReasonOf(classifyBindError(err)); got != startupFailureReasonAddressInUse {
		t.Errorf("TCP collision on %s: reason = %q, want %q", runtime.GOOS, got, startupFailureReasonAddressInUse)
	}

	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	_, err = net.ListenPacket("udp", udp.LocalAddr().String())
	if err == nil {
		t.Fatal("a second UDP listener on a held address succeeded")
	}
	if got := startupFailureReasonOf(classifyBindError(err)); got != startupFailureReasonAddressInUse {
		t.Errorf("UDP collision on %s: reason = %q, want %q", runtime.GOOS, got, startupFailureReasonAddressInUse)
	}
}

func TestLogServerExitedCarriesTheReasonAndTheError(t *testing.T) {
	record := func(err error) map[string]any {
		t.Helper()
		var buf bytes.Buffer
		logServerExited(slog.New(slog.NewJSONHandler(&buf, nil)), err)
		var m map[string]any
		if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
			t.Fatalf("server_exited is not one JSON record: %v", err)
		}
		return m
	}

	inUse := bindError(syscall.EADDRINUSE)
	m := record(classifyBindErrorFor(inUse, "linux"))
	if m["msg"] != "server_exited" || m[startupFailureReasonKey] != "address_in_use" || m["error"] != inUse.Error() {
		t.Errorf("server_exited = %v, want msg, startup_failure_reason=address_in_use and the original error", m)
	}

	other := errors.New("config: DATABASE_URL is required")
	m = record(other)
	if _, ok := m[startupFailureReasonKey]; ok {
		t.Errorf("an unclassified failure carries startup_failure_reason: %v", m)
	}
	if m["error"] != other.Error() {
		t.Errorf("error = %v, want %q", m["error"], other.Error())
	}
}

func TestStartupRejectsMalformedDatabaseURL(t *testing.T) {
	const helper = "OWNSAND_TEST_MALFORMED_DATABASE_URL_PROCESS"
	if os.Getenv(helper) == "1" {
		main()
		return
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestStartupRejectsMalformedDatabaseURL$")
	const marker = "synthetic-database-name"
	cmd.Env = []string{
		helper + "=1",
		"DATABASE_URL=postgres://test:synthetic-password@127.0.0.1:badport/" + marker,
		"LOGTO_ISSUER=http://127.0.0.1:9/oidc",
	}
	output, err := cmd.Output()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("startup exit = %v, want exit status 1", err)
	}

	var record struct {
		Message string `json:"msg"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(output, &record); err != nil {
		t.Fatalf("startup output is not one JSON record: %v", err)
	}
	const want = "parse database url: invalid connection string"
	if record.Message != "server_exited" || record.Error != want {
		t.Errorf("startup did not log server_exited with error %q", want)
	}
	if bytes.Contains(output, []byte(marker)) || bytes.Contains(exitErr.Stderr, []byte(marker)) {
		t.Error("startup output contains the synthetic database name")
	}
}

const (
	exitProcessEnv  = "OWNSAND_TEST_EXIT_PROCESS"
	listenerFailure = "listener failed"
	usageFailure    = "usage flush failed"
)

type serverExitLog struct {
	Message string `json:"msg"`
	Level   string `json:"level"`
	Error   string `json:"error"`
	Reason  string `json:"startup_failure_reason"`
}

func TestStartupExitOTLP(t *testing.T) {
	switch mode := os.Getenv(exitProcessEnv); mode {
	case "main":
		main()
		return
	case "bind", "joined", "success":
		runFinalizerProcess(t, mode)
		os.Exit(0)
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	const poolError = "DATABASE_URL pool_max_conns must be at least 2"
	cases := []struct {
		name       string
		mode       string
		env        []string
		otlp       bool
		wantExport bool
		exitCode   int
		wantError  string
		wantReason string
	}{
		{name: "pool limit", mode: "main", otlp: true, wantExport: true, exitCode: 1, wantError: poolError},
		{name: "invalid config", mode: "main", env: []string{"LOG_LEVEL=invalid"}, otlp: true, exitCode: 1, wantError: "LOG_LEVEL must be one of debug, info, warn, error"},
		{name: "telemetry init failure", mode: "main", env: []string{"OTEL_RESOURCE_ATTRIBUTES=missing"}, otlp: true, exitCode: 1, wantError: "error detecting resource: partial resource: missing value: [missing]"},
		{name: "OTLP disabled", mode: "main", exitCode: 1, wantError: poolError},
		{name: "classified bind", mode: "bind", otlp: true, wantExport: true, wantError: bindError(syscall.EADDRINUSE).Error(), wantReason: "address_in_use"},
		{name: "joined cleanup error", mode: "joined", otlp: true, wantExport: true, wantError: listenerFailure + "\n" + usageFailure},
		{name: "successful result", mode: "success", otlp: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			collector := &exitLogCollector{t: t}
			server := httptest.NewServer(collector)
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 2*telemetry.ShutdownTimeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestStartupExitOTLP$")
			cmd.Env = []string{
				exitProcessEnv + "=" + tc.mode,
				"DATABASE_URL=postgres://127.0.0.1/ownsand?pool_max_conns=1",
				"LOGTO_ISSUER=https://example.invalid/oidc",
				"LOG_LEVEL=error",
			}
			if tc.otlp {
				cmd.Env = append(cmd.Env, telemetry.EnvOTLPLogsEndpoint+"="+server.URL+"/v1/logs")
			}
			cmd.Env = append(cmd.Env, tc.env...)
			stdout, err := cmd.Output()
			if tc.exitCode == 0 && err != nil {
				t.Fatalf("child failed: %v; stdout: %s", err, stdout)
			}
			if tc.exitCode != 0 {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != tc.exitCode {
					t.Fatalf("child exit = %v, want exit status %d; stdout: %s", err, tc.exitCode, stdout)
				}
			}

			want := serverExitLog{Message: "server_exited", Level: "ERROR", Error: tc.wantError, Reason: tc.wantReason}
			if tc.wantError == "" {
				if len(stdout) != 0 {
					t.Errorf("successful finalization wrote stdout: %s", stdout)
				}
			} else {
				var got serverExitLog
				if err := json.Unmarshal(stdout, &got); err != nil {
					t.Fatalf("stdout must contain one JSON record: %v; stdout: %s", err, stdout)
				}
				if got != want {
					t.Errorf("stdout record = %+v, want %+v", got, want)
				}
			}
			collector.mu.Lock()
			defer collector.mu.Unlock()
			if tc.wantExport {
				if len(collector.records) != 1 || collector.records[0] != want {
					t.Errorf("OTLP exit records = %+v, want one %+v", collector.records, want)
				}
			} else if collector.requests != 0 {
				t.Errorf("OTLP requests = %d, want zero", collector.requests)
			}
		})
	}
}

func runFinalizerProcess(t *testing.T, mode string) {
	t.Helper()
	tel, err := telemetry.New(t.Context(), telemetry.Options{ServiceName: "ownsand-api", Level: slog.LevelError})
	if err != nil {
		t.Fatal(err)
	}
	var runErr error
	switch mode {
	case "bind":
		runErr = classifyBindErrorFor(bindError(syscall.EADDRINUSE), "linux")
	case "joined":
		runErr = errors.Join(errors.New(listenerFailure), errors.New(usageFailure))
	}
	shutdowns := 0
	finalizeRun(tel.Logger, runErr, func(ctx context.Context) error {
		shutdowns++
		deadline, ok := ctx.Deadline()
		if remaining := time.Until(deadline); !ok || remaining <= 0 || remaining > telemetry.ShutdownTimeout {
			t.Fatalf("telemetry shutdown deadline = %v, want a deadline within %v", deadline, telemetry.ShutdownTimeout)
		}
		return tel.Shutdown(ctx)
	})
	if shutdowns != 1 {
		t.Fatalf("telemetry shutdown calls = %d, want 1", shutdowns)
	}
}

type exitLogCollector struct {
	t *testing.T

	mu       sync.Mutex
	requests int
	records  []serverExitLog
}

func (c *exitLogCollector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests++
	if r.Method != http.MethodPost || r.URL.Path != "/v1/logs" {
		c.t.Errorf("unexpected OTLP request: %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected OTLP request", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(r.Body)
	var req collogspb.ExportLogsServiceRequest
	if err == nil {
		err = proto.Unmarshal(body, &req)
	}
	if err != nil {
		c.t.Errorf("cannot read OTLP request: %v", err)
		http.Error(w, "cannot read OTLP request", http.StatusBadRequest)
		return
	}
	for _, resource := range req.GetResourceLogs() {
		for _, scope := range resource.GetScopeLogs() {
			for _, record := range scope.GetLogRecords() {
				if record.GetBody().GetStringValue() != "server_exited" {
					continue
				}
				if got := record.GetSeverityNumber(); got != logspb.SeverityNumber_SEVERITY_NUMBER_ERROR {
					c.t.Errorf("OTLP severity = %v, want ERROR", got)
				}
				got := serverExitLog{Message: "server_exited", Level: record.GetSeverityText()}
				for _, attr := range record.GetAttributes() {
					switch attr.GetKey() {
					case string(semconv.ExceptionMessageKey):
						got.Error = attr.GetValue().GetStringValue()
					case startupFailureReasonKey:
						got.Reason = attr.GetValue().GetStringValue()
					}
				}
				c.records = append(c.records, got)
			}
		}
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
}
