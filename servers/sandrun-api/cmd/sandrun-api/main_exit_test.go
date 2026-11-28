// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/sandrun-api/internal/db/dbtest"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"
)

func TestServerExitLogs(t *testing.T) {
	t.Parallel()

	const (
		invalidDatabase = "parse database url: invalid connection string"
		invalidLogLevel = "LOG_LEVEL must be one of debug, info, warn, error"
	)
	for _, tc := range []struct {
		name    string
		export  bool
		clean   bool
		level   string
		wantErr string
	}{
		{name: "database failure with OTLP", export: true, level: "info", wantErr: invalidDatabase},
		{name: "configuration failure before telemetry", export: true, level: "invalid", wantErr: invalidLogLevel},
		{name: "database failure without OTLP", level: "info", wantErr: invalidDatabase},
		{name: "clean shutdown", export: true, clean: true, level: "info"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			databaseURL := "postgres://[::1"
			if tc.clean {
				if runtime.GOOS == "windows" {
					t.Skip("cannot send SIGTERM to a subprocess on Windows")
				}
				databaseURL = dbtest.NewURL(t)
			}

			var mu sync.Mutex
			var requests int
			exported := make(map[string][]*logspb.LogRecord)
			mux := http.NewServeMux()
			mux.HandleFunc("POST /v1/logs", func(w http.ResponseWriter, r *http.Request) {
				data, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read OTLP body: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				var request collogspb.ExportLogsServiceRequest
				if err := proto.Unmarshal(data, &request); err != nil {
					t.Errorf("decode OTLP body: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				requests++
				for _, resource := range request.GetResourceLogs() {
					for _, scope := range resource.GetScopeLogs() {
						for _, record := range scope.GetLogRecords() {
							msg := record.GetBody().GetStringValue()
							exported[msg] = append(exported[msg], record)
						}
					}
				}
				w.Header().Set("Content-Type", "application/x-protobuf")
			})
			collector := httptest.NewServer(mux)
			defer collector.Close()

			env := []string{
				"SANDRUN_TEST_EXIT_PROCESS=1",
				"ADDR=127.0.0.1:0",
				"DATABASE_URL=" + databaseURL,
				"RUN_MIGRATIONS=false",
				"LOG_LEVEL=" + tc.level,
				"LOGTO_ISSUER=https://example.invalid/oidc",
				"SANDRUN_SECRET_KEYS=k1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
			}
			if tc.export {
				env = append(env, "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT="+collector.URL+"/v1/logs")
			}

			logs, exitErr := runExitProcess(t, env, tc.clean)
			mu.Lock()
			defer mu.Unlock()

			exitLogs := logs["server_exited"]
			if tc.clean {
				if exitErr != nil {
					t.Errorf("clean shutdown: %v", exitErr)
				}
				if len(exitLogs) != 0 || len(exported["server_exited"]) != 0 {
					t.Error("clean shutdown emitted server_exited")
				}
				if len(logs["server_shutdown_completed"]) != 1 || len(exported["server_shutdown_completed"]) != 1 {
					t.Error("expected one server_shutdown_completed record on stdout and in OTLP")
				}
				return
			}

			var exitStatus *exec.ExitError
			if !errors.As(exitErr, &exitStatus) || exitStatus.ExitCode() != 1 {
				t.Errorf("process result = %v, want exit status 1", exitErr)
			}
			if len(exitLogs) != 1 || exitLogs[0].Level != "ERROR" || exitLogs[0].Error != tc.wantErr {
				t.Errorf("stdout server_exited = %+v, want one ERROR with %q", exitLogs, tc.wantErr)
			}

			if !tc.export || tc.level == "invalid" {
				if requests != 0 {
					t.Errorf("OTLP log requests = %d, want 0", requests)
				}
				return
			}
			records := exported["server_exited"]
			if len(records) != 1 || records[0].GetSeverityNumber() != logspb.SeverityNumber_SEVERITY_NUMBER_ERROR {
				t.Fatalf("OTLP server_exited = %v, want one ERROR", records)
			}
			var exceptionMessage string
			for _, attr := range records[0].GetAttributes() {
				if attr.GetKey() == "exception.message" {
					exceptionMessage = attr.GetValue().GetStringValue()
				}
			}
			if exceptionMessage != tc.wantErr {
				t.Errorf("OTLP exception.message = %q, want %q", exceptionMessage, tc.wantErr)
			}
		})
	}
}

type exitLog struct {
	Msg   string `json:"msg"`
	Level string `json:"level"`
	Error string `json:"error"`
	Addr  string `json:"addr"`
}

func runExitProcess(t *testing.T, env []string, clean bool) (map[string][]exitLog, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestServerExitProcess$")
	cmd.Env = env
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	logs := make(map[string][]exitLog)
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		var log exitLog
		if err := json.Unmarshal(scanner.Bytes(), &log); err != nil {
			t.Fatalf("decode stdout record %q: %v", scanner.Text(), err)
		}
		logs[log.Msg] = append(logs[log.Msg], log)
		if clean && log.Msg == "server_started" {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+log.Addr+"/healthz", nil)
			if err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("health status = %d, want 200", resp.StatusCode)
			}
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return logs, cmd.Wait()
}

// TestServerExitProcess runs the production entry point with an isolated
// environment and logger in a child process.
func TestServerExitProcess(t *testing.T) {
	if os.Getenv("SANDRUN_TEST_EXIT_PROCESS") == "1" {
		main()
		os.Exit(0)
	}
}
