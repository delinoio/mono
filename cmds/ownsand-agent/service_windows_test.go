// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"context"
	"errors"
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
	const rawLevel = "private-fixture-level"
	const rawPath = "private-fixture-state"
	wantSetting := "invalid OWNSAND_AGENT_LOG_LEVEL: expected debug, info, warn or error"
	_, cfgErr := config.FromEnv(func(name string) (string, bool) {
		if name == config.EnvLogLevel {
			return rawLevel, true
		}
		return "", false
	})
	if cfgErr == nil {
		t.Fatal("invalid log level accepted")
	}
	for _, tc := range []struct {
		name       string
		blockMkdir bool
		blockOpen  bool
		eventError error
		wantEvent  string
	}{
		{name: "directory creation fails", blockMkdir: true, wantEvent: "could not create the service state directory"},
		{name: "file open fails", blockOpen: true, wantEvent: "could not open agent.log"},
		{name: "Event Log write fails", blockOpen: true, eventError: errors.New("write failed"), wantEvent: "could not open agent.log"},
		{name: "file log works"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), rawPath)
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
			if code := runUnderServiceManager(config.Config{StateDir: dir}, cfgErr, dispatch, writeEvent, nil); code != exitUsage {
				t.Fatalf("runUnderServiceManager = %d, want %d", code, exitUsage)
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
			if strings.Count(log, `"msg":"invalid configuration"`) != 1 || !strings.Contains(log, wantSetting) || strings.Contains(log, rawLevel) {
				t.Fatalf("unexpected log contents: %s", log)
			}
		})
	}
}

func TestConfigErrorStateDir(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"set", map[string]string{config.EnvStateDir: ` C:\agent-state `}, `C:\agent-state`},
		{"blank", map[string]string{config.EnvStateDir: "  "}, config.DefaultStateDir()},
		{"unset", nil, config.DefaultStateDir()},
	} {
		lookup := func(k string) (string, bool) { v, ok := tc.env[k]; return v, ok }
		if got := configErrorStateDir(lookup); got != tc.want {
			t.Errorf("%s: configErrorStateDir = %q, want %q", tc.name, got, tc.want)
		}
	}
}
