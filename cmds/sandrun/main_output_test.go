// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin

package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/cmds/internal/credstore"
	"github.com/delinoio/mono/cmds/sandrun/internal/cli"
	"github.com/delinoio/mono/go/sandrun"
	"github.com/delinoio/mono/go/sandrun/sandruntest"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
)

func TestClosedOutputPipeForHelpAndVersion(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"help", []string{"help"}, "Usage: sandrun"},
		{"global help flag", []string{"--help"}, "Usage: sandrun"},
		{"command help flag", []string{"session", "exec", "-h"}, "Usage: sandrun session exec"},
		{"version", []string{"version"}, cli.Version + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := sandRunCommand(t, "", tc.args...)
			out, err := cmd.Output()
			if err != nil || !strings.HasPrefix(string(out), tc.want) {
				t.Fatalf("writable stdout = %q, error = %v, want %q and exit 0", out, err, tc.want)
			}
			checkClosedOutputPipe(t, sandRunCommand(t, "", tc.args...), false)
		})
	}

	t.Run("no arguments", func(t *testing.T) {
		var stderr bytes.Buffer
		cmd := sandRunCommand(t, "")
		cmd.Stderr = &stderr
		checkNormalExit(t, cmd, cli.ExitUsage)
		if !strings.Contains(stderr.String(), "Usage: sandrun") {
			t.Fatalf("stderr = %q, want usage", stderr.String())
		}
	})
}

func TestClosedOutputPipeForGitHubInstallURL(t *testing.T) {
	srv := sandruntest.New()
	t.Cleanup(srv.Close)
	org := srv.AddOrganization("fixture-org").GetOrganizationId()
	cfg := t.TempDir()
	store := credstore.Store{Dir: cfg, Program: "sandrun"}
	if err := store.Save(&credstore.Credentials{
		LogtoEndpoint: "https://logto.example.com", AppID: "app", Resource: cli.DefaultLogtoResource,
		RefreshToken: "refresh", AccessToken: srv.AccessToken, AccessTokenExpiry: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			cmd := sandRunCommand(t, srv.URL, "--org", org, "--output", format, "github", "install-url")
			cmd.Env = append(cmd.Env, cli.EnvAPIKey+"=", cli.EnvConfigDir+"="+cfg)
			checkClosedOutputPipe(t, cmd, false)
		})
	}
}

func TestClosedOutputPipeForSession(t *testing.T) {
	srv := sandruntest.New()
	t.Cleanup(srv.Close)
	org := srv.AddOrganization("fixture-org").GetOrganizationId()
	profile := srv.AddDevSessionProfile(org, "linux", sandrunv1.GuestOs_GUEST_OS_LINUX, t.TempDir())
	client, err := sandrun.New(sandrun.Options{BaseURL: srv.URL, APIKey: srv.APIKey})
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.DevSessions().CreateDevSession(t.Context(), connect.NewRequest(&sandrunv1.CreateDevSessionRequest{
		OrganizationId: org, RunnerProfileId: profile.GetRunnerProfileId(),
	}))
	if err != nil {
		t.Fatal(err)
	}
	session := res.Msg.GetDevSession().GetDevSessionId()
	runExec := func(t *testing.T, script string) string {
		t.Helper()
		reqs := len(srv.ExecRequests())
		cmd := sandRunCommand(t, srv.URL, "--org", org, "session", "exec", session, "--", "sh", "-c", script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("prepare attach: %v, output %q", err, out)
		}
		requests := srv.ExecRequests()
		if len(requests) != reqs+1 {
			t.Fatalf("exec added %d requests, want 1", len(requests)-reqs)
		}
		return requests[reqs].GetExecId()
	}

	for _, tc := range []struct {
		name, format, script string
		closeStderr          bool
	}{
		{name: "stdout", format: "text", script: "printf out"},
		{name: "stderr", format: "text", script: "printf err >&2", closeStderr: true},
		{name: "json", format: "json", script: "printf out; printf err >&2"},
	} {
		t.Run("exec/"+tc.name, func(t *testing.T) {
			cmd := sandRunCommand(t, srv.URL, "--org", org, "--output", tc.format, "session", "exec", session, "--", "sh", "-c", tc.script)
			checkClosedOutputPipe(t, cmd, tc.closeStderr)
		})
		t.Run("attach/"+tc.name, func(t *testing.T) {
			execID := runExec(t, tc.script)
			cmd := sandRunCommand(t, srv.URL, "--org", org, "--output", tc.format, "session", "attach", session, execID)
			checkClosedOutputPipe(t, cmd, tc.closeStderr)
		})
	}

	t.Run("attach/terminal JSON", func(t *testing.T) {
		silentExecID := runExec(t, ":")
		args := []string{"--org", org, "--output", "json", "session", "attach", session, silentExecID}
		out, err := sandRunCommand(t, srv.URL, args...).Output()
		if err != nil || bytes.Count(out, []byte("\n")) != 1 || !bytes.Contains(out, []byte(`"event":"exited"`)) {
			t.Fatalf("silent attach wrote %q, error = %v, want one terminal JSON event", out, err)
		}
		checkClosedOutputPipe(t, sandRunCommand(t, srv.URL, args...), false)
	})
}

func checkClosedOutputPipe(t *testing.T, cmd *exec.Cmd, closeStderr bool) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	// Close the reader before starting the CLI so the first write fails.
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}

	var captured bytes.Buffer
	if closeStderr {
		cmd.Stdout, cmd.Stderr = &captured, writer
	} else {
		cmd.Stdout, cmd.Stderr = writer, &captured
	}
	checkNormalExit(t, cmd, cli.ExitError)
	if closeStderr {
		if captured.Len() != 0 {
			t.Fatalf("stdout = %q, want no output", captured.String())
		}
	} else {
		want := fmt.Sprintf("error: write %s: %s\n", os.Stdout.Name(), syscall.EPIPE)
		if captured.String() != want {
			t.Fatalf("stderr = %q, want only %q", captured.String(), want)
		}
	}
}

func checkNormalExit(t *testing.T, cmd *exec.Cmd, want int) {
	t.Helper()
	var exit *exec.ExitError
	if err := cmd.Run(); !errors.As(err, &exit) || exit.ExitCode() != want {
		t.Fatalf("%v exited with %v, want normal exit %d (stdout %v, stderr %v)",
			cmd.Args[1:], err, want, cmd.Stdout, cmd.Stderr)
	}
}
