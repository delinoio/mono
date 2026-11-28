// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/accounts-api/internal/config"
	"github.com/delinoio/mono/servers/internal/telemetry"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	collectorpb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"
)

const (
	otelResourceAttributes = "OTEL_RESOURCE_ATTRIBUTES"
	otelServiceName        = "OTEL_SERVICE_NAME"
)

// runEnv returns the configuration that lets run start without external
// services. The database pool opens lazily and RUN_MIGRATIONS=false skips the
// migrations. Empty OpenTelemetry settings keep resource detection and export
// independent of the test process environment.
func runEnv(metricsAddr string) map[string]string {
	return map[string]string{
		"DATABASE_URL":                 "postgres://accounts@127.0.0.1:1/accounts?connect_timeout=1",
		"RUN_MIGRATIONS":               "false",
		"ADDR":                         "127.0.0.1:0",
		"METRICS_ADDR":                 metricsAddr,
		config.EnvLogLevel:             "error",
		"LOGTO_ISSUER":                 "http://127.0.0.1:1/oidc",
		"LOGTO_MANAGEMENT_ENDPOINT":    "http://127.0.0.1:1",
		"LOGTO_M2M_CLIENT_ID":          "run-test",
		"LOGTO_M2M_CLIENT_SECRET":      "run-test-secret",
		"RESEND_API_KEY":               "re_run_test",
		"EMAIL_FROM":                   "billing@example.com",
		"PADDLE_ENVIRONMENT":           "sandbox",
		"PADDLE_API_KEY":               "pdl_run_test",
		"PADDLE_WEBHOOK_SECRET":        "pdl_ntfset_run_test",
		"PADDLE_SUBSCRIPTION_PRICE_ID": "pri_01aaaaaaaaaaaaaaaaaaaaaaaa",
		"PADDLE_TOP_UP_PRODUCT_ID":     "pro_01bbbbbbbbbbbbbbbbbbbbbbbb",

		otelResourceAttributes:           "",
		otelServiceName:                  "",
		telemetry.EnvOTLPEndpoint:        "",
		telemetry.EnvOTLPMetricsEndpoint: "",
		telemetry.EnvOTLPLogsEndpoint:    "",
	}
}

func setRunEnv(t *testing.T, metricsAddr string) {
	t.Helper()
	for name, value := range runEnv(metricsAddr) {
		t.Setenv(name, value)
	}
}

// TestRunFailsOnMetricsBindErrorBeforeStartingWorker verifies that a metrics
// listener that cannot be bound fails run before the billing worker starts, so
// no Paddle call can begin after run has returned.
func TestRunFailsOnMetricsBindErrorBeforeStartingWorker(t *testing.T) {
	inheritedResources := map[string]string{
		otelResourceAttributes: "invalid_fixture_attribute",
		otelServiceName:        "inherited-accounts-test",
	}
	for name, value := range inheritedResources {
		t.Setenv(name, value)
	}

	held := newTestListener(t)
	tests := []struct {
		name        string
		metricsAddr string
	}{
		{name: "address in use", metricsAddr: held.Addr().String()},
		{name: "host that does not resolve", metricsAddr: "nonexistent.invalid:9100"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setRunEnv(t, tt.metricsAddr)
			assertRunMetricsBindError(t)
			if got := os.Getenv(otelServiceName); got != "" {
				t.Errorf("%s = %q inside the fixture; want empty", otelServiceName, got)
			}
		})
		for name, want := range inheritedResources {
			if got := os.Getenv(name); got != want {
				t.Errorf("after %s, %s = %q; want %q", tt.name, name, got, want)
			}
		}
	}
}

func TestRunIgnoresInheritedOTLPEndpoints(t *testing.T) {
	t.Setenv(otelResourceAttributes, "fixture.source=inherited")
	t.Setenv(otelServiceName, "inherited-accounts-test")
	held := newTestListener(t)
	endpoints := []string{
		telemetry.EnvOTLPEndpoint,
		telemetry.EnvOTLPMetricsEndpoint,
		telemetry.EnvOTLPLogsEndpoint,
	}
	for _, endpoint := range endpoints {
		t.Run(endpoint, func(t *testing.T) {
			var requests atomic.Int64
			collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			defer collector.Close()
			for _, name := range endpoints {
				t.Setenv(name, "")
			}
			t.Setenv(endpoint, collector.URL)

			t.Run("bind", func(t *testing.T) {
				setRunEnv(t, held.Addr().String())
				// Emit the migration-skip log so an enabled log exporter has
				// a record to flush during shutdown.
				t.Setenv(config.EnvLogLevel, "info")
				assertRunMetricsBindError(t)
			})
			if got := requests.Load(); got != 0 {
				t.Errorf("collector received %d requests; want 0", got)
			}
			if got := os.Getenv(endpoint); got != collector.URL {
				t.Errorf("%s = %q after the fixture; want %q", endpoint, got, collector.URL)
			}
		})
	}
}

func assertRunMetricsBindError(t *testing.T) {
	t.Helper()
	originalLogger := slog.Default()
	originalLogWriter, originalLogFlags := log.Writer(), log.Flags()
	t.Cleanup(func() {
		slog.SetDefault(originalLogger)
		log.SetOutput(originalLogWriter)
		log.SetFlags(originalLogFlags)
	})

	dispatches := 0
	dispatchWorker := func(func() error) <-chan error {
		dispatches++
		result := make(chan error, 1)
		result <- nil
		return result
	}

	err := run(dispatchWorker)
	if err == nil {
		t.Fatal("run succeeded; want the metrics listen error")
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Op != "listen" {
		t.Fatalf("run error = %v; want a listen error", err)
	}
	if dispatches != 0 {
		t.Errorf("billing worker dispatches = %d; want 0 after a metrics listen error", dispatches)
	}
}

type exitRecord struct {
	Message string `json:"msg"`
	Level   string `json:"level"`
	Error   string `json:"error"`
}

func TestMainExitLogs(t *testing.T) {
	const childEnv = "ACCOUNTS_API_TEST_MAIN_EXIT_LOGS"
	if os.Getenv(childEnv) == "1" {
		main()
		os.Exit(0)
	}

	held := newTestListener(t)
	addr := held.Addr().String()
	// Get the native bind error text because it differs between platforms.
	collision, err := net.Listen("tcp", addr)
	if err == nil {
		_ = collision.Close()
		t.Fatal("second listener bound the occupied address")
	}
	bindError := err.Error()

	for _, tc := range []struct {
		name, addr, level, wantError string
		configureOTLP, wantExport    bool
	}{
		{name: "bind failure", addr: addr, level: "error", wantError: bindError, configureOTLP: true, wantExport: true},
		{name: "invalid config", addr: addr, level: "bogus", wantError: `LOG_LEVEL must be one of debug, info, warn, error; got "bogus"`, configureOTLP: true},
		{name: "OTLP disabled", addr: addr, level: "error", wantError: bindError},
		{name: "clean shutdown", addr: "127.0.0.1:0", level: "info", configureOTLP: true, wantExport: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cleanShutdown := tc.wantError == ""
			if cleanShutdown && runtime.GOOS == "windows" {
				t.Skip("sending SIGTERM to a subprocess requires POSIX signals")
			}

			collector := &exitLogCollector{t: t}
			receiver := httptest.NewServer(collector)
			defer receiver.Close()

			env := runEnv("")
			env["ADDR"] = tc.addr
			env[config.EnvLogLevel] = tc.level
			if tc.configureOTLP {
				env[telemetry.EnvOTLPLogsEndpoint] = receiver.URL + "/v1/logs"
			}
			env[childEnv] = "1"

			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestMainExitLogs$")
			// Use only the fixture environment, so inherited OTEL settings,
			// proxies, and service credentials cannot affect the subprocess.
			cmd.Env = make([]string, 0, len(env))
			for name, value := range env {
				cmd.Env = append(cmd.Env, name+"="+value)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			}()

			var stdoutExits []exitRecord
			sentSignal := false
			scanner := bufio.NewScanner(stdout)
			for scanner.Scan() {
				var record exitRecord
				if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
					t.Fatalf("invalid JSON log %q: %v", scanner.Text(), err)
				}
				switch record.Message {
				case "server_exited":
					stdoutExits = append(stdoutExits, record)
				case "server_started":
					if cleanShutdown && !sentSignal {
						if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
							t.Fatal(err)
						}
						sentSignal = true
					}
				}
			}
			if err := scanner.Err(); err != nil {
				t.Fatal(err)
			}
			err = cmd.Wait()
			wantStatus := 1
			var wantExits []exitRecord
			if cleanShutdown {
				wantStatus = 0
				if !sentSignal {
					t.Error("process did not log server_started before exiting")
				}
			} else {
				wantExits = []exitRecord{{Message: "server_exited", Level: "ERROR", Error: tc.wantError}}
			}
			if status := cmd.ProcessState.ExitCode(); status != wantStatus {
				t.Errorf("exit status = %d, want %d (wait error: %v, stderr: %s)", status, wantStatus, err, &stderr)
			}
			if !slices.Equal(stdoutExits, wantExits) {
				t.Errorf("stdout exit records = %+v, want %+v", stdoutExits, wantExits)
			}

			collector.mu.Lock()
			defer collector.mu.Unlock()
			if tc.wantExport {
				if !slices.Equal(collector.exits, wantExits) {
					t.Errorf("OTLP exit records = %+v, want %+v", collector.exits, wantExits)
				}
				if cleanShutdown && !collector.sawStartup {
					t.Error("collector did not receive server_started")
				}
			} else if collector.requests != 0 {
				t.Errorf("OTLP requests = %d, want zero", collector.requests)
			}
		})
	}
}

type exitLogCollector struct {
	t *testing.T

	mu         sync.Mutex
	requests   int
	exits      []exitRecord
	sawStartup bool
}

func (c *exitLogCollector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests++
	if r.Method != http.MethodPost || r.URL.Path != "/v1/logs" {
		c.t.Errorf("unexpected OTLP request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(r.Body)
	var req collectorpb.ExportLogsServiceRequest
	if err == nil {
		err = proto.Unmarshal(body, &req)
	}
	if err != nil {
		c.t.Errorf("cannot read OTLP request: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	for _, resource := range req.GetResourceLogs() {
		for _, scope := range resource.GetScopeLogs() {
			for _, record := range scope.GetLogRecords() {
				switch message := record.GetBody().GetStringValue(); message {
				case "server_started":
					c.sawStartup = true
				case "server_exited":
					if severity := record.GetSeverityNumber(); severity != logspb.SeverityNumber_SEVERITY_NUMBER_ERROR {
						c.t.Errorf("OTLP exit severity = %v, want ERROR", severity)
					}
					exit := exitRecord{Message: message, Level: record.GetSeverityText()}
					for _, attr := range record.GetAttributes() {
						if attr.GetKey() == string(semconv.ExceptionMessageKey) {
							exit.Error = attr.GetValue().GetStringValue()
						}
					}
					c.exits = append(c.exits, exit)
				}
			}
		}
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
}
