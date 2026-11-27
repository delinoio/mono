// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/config"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/service"
)

func checkServiceEnvironment(definition, name, want string) error {
	for line := range strings.SplitSeq(definition, "\n") {
		if !strings.HasPrefix(line, "reg.exe add ") {
			continue
		}
		args, err := windows.DecomposeCommandLine(line)
		if err != nil {
			return fmt.Errorf("decode service registry command: %w", err)
		}
		if len(args) != 12 || args[3] != "/v" || args[4] != service.WindowsEnvironmentValue ||
			args[5] != "/t" || args[6] != "REG_MULTI_SZ" || args[9] != "/d" {
			return fmt.Errorf("unexpected service registry command: %q", line)
		}
		// The CLI fixture sets only the variable under test, so /d has one entry.
		if expected := name + "=" + want; args[10] != expected {
			return fmt.Errorf("service registry environment = %q, want %q", args[10], expected)
		}
		return nil
	}
	return errors.New("missing service registry environment command")
}

func TestServiceMigrationPreservesRunArguments(t *testing.T) {
	launcher := `C:\Program Files\OwnSand Agent\ownsand-service.exe`
	old := `"D:\Agent Files\ownsand-agent.exe" run --state-dir "D:\Agent State" --no-color`
	path, migrated, err := migratedWindowsServicePath(old, launcher)
	if err != nil || migrated {
		t.Fatalf("migrate %q = migrated %v, error %v", old, migrated, err)
	}
	result := windows.UTF16PtrToString(path)
	args, err := windows.DecomposeCommandLine(result)
	want := []string{launcher, "service", "launch", "run", "--state-dir", `D:\Agent State`, "--no-color"}
	if err != nil || !reflect.DeepEqual(args, want) {
		t.Fatalf("migrated command line = %q, %v, want %q", args, err, want)
	}
	if _, migrated, err := migratedWindowsServicePath(result, launcher); err != nil || !migrated {
		t.Fatalf("repeat migration = migrated %v, error %v", migrated, err)
	}
}

func TestServiceLauncherDirectorySecurity(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("creating the launcher directory needs an elevated administrator")
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, "launcher")
	for range 2 {
		if err := createLauncherDirectory(dir); err != nil {
			t.Fatal(err)
		}
	}
	other := filepath.Join(parent, "existing")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := createLauncherDirectory(other); err == nil {
		t.Fatal("accepted an existing directory without the launcher DACL")
	}
}

// executeHandler drives handler.Execute with a fake runner. When stop is
// true, the test sends a stop request and the runner returns its outcome
// after the context ends; otherwise the runner returns at once.
func executeHandler(t *testing.T, code int, updated, stop bool) (h *handler, specific bool, exit uint32) {
	t.Helper()
	h = &handler{run: func(ctx context.Context, _ config.Config) (int, bool) {
		if stop {
			<-ctx.Done()
		}
		return code, updated
	}}
	requests := make(chan svc.ChangeRequest, 1)
	status := make(chan svc.Status, 8)
	if stop {
		requests <- svc.ChangeRequest{Cmd: svc.Stop}
	}
	type result struct {
		specific bool
		exit     uint32
	}
	res := make(chan result, 1)
	go func() {
		s, e := h.Execute(nil, requests, status)
		res <- result{s, e}
	}()
	select {
	case r := <-res:
		return h, r.specific, r.exit
	case <-time.After(10 * time.Second):
		t.Fatal("Execute did not return")
	}
	return nil, false, 0
}

func TestHandlerReportsTheRestartStatus(t *testing.T) {
	for _, tc := range []struct {
		name          string
		code          int
		updated, stop bool
		specific      bool
		exit          uint32
	}{
		{"installed update", 0, true, false, true, 75},
		{"error", 1, false, false, true, 1},
		{"not enrolled or removed", 3, false, false, false, 0},
		{"clean stop", 0, false, false, false, 0},
		{"stop request while the agent later fails", 1, false, true, false, 0},
	} {
		h, specific, exit := executeHandler(t, tc.code, tc.updated, tc.stop)
		if specific != tc.specific || exit != tc.exit {
			t.Errorf("%s: Execute = (%v, %d), want (%v, %d)", tc.name, specific, exit, tc.specific, tc.exit)
		}
		// The public exit code of `run` does not change with the reported status.
		if h.code != tc.code {
			t.Errorf("%s: public exit code = %d, want %d", tc.name, h.code, tc.code)
		}
	}
}

func TestStartupDiagnosticForInvalidConfiguration(t *testing.T) {
	clearAgentEnv(t)
	const rawLevel = "fixture-invalid-level"
	const rawPath = "Private Fixture State"
	t.Setenv(config.EnvLogLevel, rawLevel)
	wantSetting := "invalid OWNSAND_AGENT_LOG_LEVEL: expected debug, info, warn or error"
	buildDirectArgs := func(dir string) []string {
		return []string{"run", "--state-dir", dir}
	}
	buildLauncherArgs := func(dir string) []string {
		return []string{"service", "launch", "run", "--state-dir=" + dir}
	}
	for _, tc := range []struct {
		name       string
		args       func(string) []string
		fallback   bool
		blockMkdir bool
		blockOpen  bool
		eventError error
		wantEvent  string
	}{
		{name: "direct service", args: buildDirectArgs},
		{name: "migrated service", args: buildLauncherArgs},
		{name: "directory creation fails", args: buildDirectArgs, blockMkdir: true, wantEvent: "could not create the service state directory"},
		{name: "file open fails", args: buildLauncherArgs, blockOpen: true, wantEvent: "could not open agent.log"},
		{name: "Event Log write fails", args: buildDirectArgs, blockOpen: true, eventError: errors.New("write failed"), wantEvent: "could not open agent.log"},
		{name: "missing flag", args: func(string) []string { return []string{"run"} }, fallback: true},
		{name: "blank value", args: func(string) []string { return []string{"run", "--state-dir=  "} }, fallback: true},
		{name: "missing value", args: func(string) []string { return []string{"run", "--state-dir"} }, fallback: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			explicit := filepath.Join(root, rawPath)
			fallback := filepath.Join(root, "Fallback State")
			t.Setenv(config.EnvStateDir, " "+fallback+" ")
			dir := explicit
			otherDir := fallback
			if tc.fallback {
				dir, otherDir = fallback, explicit
			}
			path := filepath.Join(dir, "agent.log")
			if tc.blockMkdir {
				if err := os.WriteFile(dir, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.blockOpen {
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			dispatch := func(name string, h svc.Handler) error {
				if name != service.Name {
					t.Fatalf("dispatched service %q, want %q", name, service.Name)
				}
				status := make(chan svc.Status, 8)
				specific, exit := h.Execute(nil, make(chan svc.ChangeRequest), status)
				if !specific || exit != exitUsage {
					t.Fatalf("Execute = (%v, %d), want (true, %d)", specific, exit, exitUsage)
				}
				return nil
			}
			var events []string
			writeEvent := func(message string) error {
				events = append(events, message)
				return tc.eventError
			}
			var stderr bytes.Buffer
			report := func(cfgErr error, explicitStateDir string, _ io.Writer) int {
				return runServiceConfigError(cfgErr, explicitStateDir, dispatch, writeEvent)
			}
			code := runWithConfigErrorHandler(tc.args(explicit), nil, io.Discard, &stderr, report)
			if code != exitUsage || stderr.Len() != 0 {
				t.Fatalf("run = %d, want %d, stderr = %q", code, exitUsage, stderr.String())
			}
			if _, err := os.Stat(otherDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unexpected use of directory %q: %v", otherDir, err)
			}
			if tc.wantEvent != "" {
				if len(events) != 1 || !strings.HasPrefix(events[0], tc.wantEvent) || !strings.Contains(events[0], wantSetting) {
					t.Fatalf("events = %q, want one event with %q and %q", events, tc.wantEvent, wantSetting)
				}
				if strings.Contains(events[0], rawPath) || strings.Contains(events[0], rawLevel) {
					t.Fatalf("event exposes an environment value: %q", events[0])
				}
				return
			}
			if len(events) != 0 {
				t.Fatalf("events = %q with a working log file", events)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			log := string(data)
			if strings.Count(log, `"msg":"invalid configuration"`) != 1 || !strings.Contains(log, wantSetting) || strings.Contains(log, rawPath) || strings.Contains(log, rawLevel) {
				t.Fatalf("unexpected log contents: %s", log)
			}
		})
	}
}

func TestConfigErrorStateDir(t *testing.T) {
	for _, tc := range []struct {
		name     string
		explicit string
		env      map[string]string
		want     string
	}{
		{"explicit", `C:\Agent State`, map[string]string{config.EnvStateDir: `C:\Fallback State`}, `C:\Agent State`},
		{"set", "", map[string]string{config.EnvStateDir: ` C:\agent-state `}, `C:\agent-state`},
		{"blank", "", map[string]string{config.EnvStateDir: "  "}, config.DefaultStateDir()},
		{"unset", "", nil, config.DefaultStateDir()},
	} {
		lookup := func(k string) (string, bool) { v, ok := tc.env[k]; return v, ok }
		if got := configErrorStateDir(tc.explicit, lookup); got != tc.want {
			t.Errorf("%s: configErrorStateDir = %q, want %q", tc.name, got, tc.want)
		}
	}
}
