// SPDX-License-Identifier: FSL-1.1-ALv2

package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/delinoio/mono/go/ownsand"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/agent"
	"github.com/delinoio/mono/servers/sandrun-api/internal/agent/cursor"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient"
	"github.com/delinoio/mono/servers/sandrun-api/internal/work"
)

const recordEnv = "SANDRUN_ARGUMENT_GUARD_PROBE_PATH"

func TestMain(m *testing.M) {
	// The test binary acts as an inert native program in the guest shell.
	if path := os.Getenv(recordEnv); path != "" {
		data, err := json.Marshal(os.Args[1:])
		if err == nil {
			err = os.WriteFile(path, data, 0600)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type localGuest struct {
	ownsandclient.Client
	execCalls int
}

func (g *localGuest) Exec(ctx context.Context, _ string, p ownsand.ExecParams) (*ownsand.ExecResult, error) {
	g.execCalls++
	cmd := exec.CommandContext(ctx, p.Command[0], p.Command[1:]...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("local guest shell: %w: %s", err, output)
	}
	// Report the fixture's start event after the recorder has exited.
	return nil, p.OnEvent(ownsand.ExecEvent{Kind: ownsand.ExecEventStarted})
}

func TestArgumentGuardNativeProbe(t *testing.T) {
	isWindows := runtime.GOOS == "windows"
	guestOS := sandrunv1.GuestOs_GUEST_OS_LINUX
	home := os.Getenv("HOME")
	if isWindows {
		guestOS = sandrunv1.GuestOs_GUEST_OS_WINDOWS
		home = os.Getenv("USERPROFILE")
		version, err := exec.CommandContext(t.Context(), "powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
			"if ($PSVersionTable.PSVersion.Major -ne 5 -or $PSVersionTable.PSVersion.Minor -ne 1) { exit 1 }; $PSVersionTable.PSVersion.ToString()").CombinedOutput()
		if err != nil {
			t.Fatalf("Windows PowerShell 5.1 required: %v (%s)", err, version)
		}
		t.Logf("Windows PowerShell version %s", version)
	} else if runtime.GOOS != "linux" {
		t.Fatalf("run the probe on Linux or Windows, got %s", runtime.GOOS)
	}
	if home == "" {
		t.Fatal("the validation account must have a home directory")
	}
	recorder, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, pool string
		reject     bool
	}{
		{name: "previously stored pool", pool: `team"a`, reject: isWindows},
		{name: "supported pool", pool: "Bob's team’s pool"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			record := filepath.Join(dir, "argv.json")
			credential := filepath.Join(dir, agent.CredentialFile)
			if err := os.WriteFile(credential, []byte("fixture-key"), 0600); err != nil {
				t.Fatal(err)
			}
			program := cursor.Program(dir, &agent.Claim{Scope: tc.pool, WorkerID: "fixture-worker"}, 600)
			program.Args[0] = recorder
			program.Env = append(program.Env, [2]string{recordEnv, record})
			client := &localGuest{}
			guest := &work.Guest{Client: client, Runner: &work.Runner{GuestOS: guestOS}}
			start, err := agent.StartProgram(t.Context(), guest, program)
			if tc.reject {
				const message = "a runner argument contains a double quote, which Windows PowerShell 5.1 cannot pass to a native program"
				var step *agent.StepError
				if !errors.As(err, &step) || step.Step != "start the runner program" || step.Code != -1 || step.Msg != message ||
					start.ProgramExecID != "" || client.execCalls != 0 {
					t.Fatalf("start = %+v, error = %#v, exec calls = %d", start, err, client.execCalls)
				}
				if _, err := os.Stat(record); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("wanted no argument record, stat error = %v", err)
				}
				t.Logf("rejected stored pool before guest exec: %s", step)
				return
			}
			if err != nil || start.ProgramExecID == "" || client.execCalls != 1 {
				log, _ := os.ReadFile(filepath.Join(dir, agent.LogFile))
				t.Fatalf("start = %+v, error = %v, exec calls = %d, log = %q", start, err, client.execCalls, log)
			}
			data, err := os.ReadFile(record)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			want := []string{"worker", "--pool", tc.pool, "--worker-dir", home, "--idle-release-timeout", "600", "start"}
			if !slices.Equal(got, want) {
				t.Fatalf("native argv = %q, want %q", got, want)
			}
			t.Logf("native argv = %q", got)
		})
	}
}
