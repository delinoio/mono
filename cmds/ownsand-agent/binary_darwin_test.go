// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/testcp"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestBinaryWithProcessRuntimeDarwin(t *testing.T) {
	bin := buildProcessRuntimeBinaries(t)
	for _, stage := range []string{"startup", "running", "paused", "delete"} {
		t.Run(stage, func(t *testing.T) {
			cp := testcp.New(t)
			stateDir := t.TempDir()
			fixtureDir := t.TempDir()
			guest := filepath.Join(fixtureDir, "fixture guest")
			marker := filepath.Join(fixtureDir, "helper")
			// Capture the selected executable's launch settings before exec
			// replaces it with the real guest in the same process. The
			// startup case stops before opening a readiness socket.
			const wrapper = `#!/bin/sh
set -eu
printf '%s\n' "$$" "$PPID" "$(pwd -P)" "$HOME" "$OWNSAND_TEST_GUEST_PROBE" "$@" > "$OWNSAND_TEST_HELPER_MARKER.tmp"
/bin/mv "$OWNSAND_TEST_HELPER_MARKER.tmp" "$OWNSAND_TEST_HELPER_MARKER"
if [ "$OWNSAND_TEST_GUEST_PROBE" = startup ]; then
  kill -STOP "$$"
fi
exec "$OWNSAND_TEST_REAL_GUEST" "$@"
`
			if err := os.WriteFile(guest, []byte(wrapper), 0o700); err != nil {
				t.Fatal(err)
			}
			env := append(processRuntimeEnv(stateDir, guest),
				"OWNSAND_TEST_HELPER_MARKER="+marker,
				"OWNSAND_TEST_REAL_GUEST="+filepath.Join(bin, "ownsand-guest"),
				"OWNSAND_TEST_GUEST_PROBE="+stage,
			)
			enrollProcessRuntimeAgent(t, bin, cp, env)
			agent := startProcessRuntimeAgent(t, bin, env)
			agent.waitForReport(t, cp, 0)
			id, _ := uuidv7.NewString()
			sandbox := filepath.Join(stateDir, "vms", id)
			guestState := filepath.Join(sandbox, "guest-state")
			t.Cleanup(func() {
				// The pre-fix binary leaves the helper alive. Kill only
				// the fixture whose command line still has our state path.
				info, _ := os.ReadFile(marker)
				pid, err := strconv.Atoi(strings.SplitN(string(info), "\n", 2)[0])
				if err == nil && pid > 1 && strings.Contains(processField(pid, "command"), guestState) {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			})
			createID := sendProcessRuntimeCreate(t, cp, id)
			deadline := time.Now().Add(binaryAgentTimeout)
			var info []byte
			for time.Now().Before(deadline) {
				var err error
				info, err = os.ReadFile(marker)
				if err == nil {
					break
				}
				if !os.IsNotExist(err) {
					t.Fatal(err)
				}
				time.Sleep(20 * time.Millisecond)
			}
			if len(info) == 0 {
				t.Fatalf("fixture guest did not record its launch\n%s", agent.logs.String())
			}
			socketDir := processRuntimeSocketDir(t, sandbox)
			cwd, err := filepath.EvalSymlinks(filepath.Join(sandbox, "root"))
			if err != nil {
				t.Fatal(err)
			}
			args := strings.Split(strings.TrimSuffix(string(info), "\n"), "\n")
			want := []string{cwd, filepath.Join(sandbox, "root"), stage,
				"--listen", "unix://" + filepath.Join(socketDir, "g.sock"),
				"--state-dir", guestState, "--clock", "ignore"}
			if len(args) < 2 || !slices.Equal(args[2:], want) {
				t.Fatalf("fixture launch = %q; want PID, PPID, %q", args, want)
			}
			pid, pidErr := strconv.Atoi(args[0])
			parent, parentErr := strconv.Atoi(args[1])
			if pidErr != nil || parentErr != nil || pid <= 1 || parent <= 1 {
				t.Fatalf("fixture PID and PPID = %q", args[:2])
			}
			if pgid, err := syscall.Getpgid(pid); err != nil || pgid != pid {
				t.Fatalf("fixture PID %d, PGID %d, error %v", pid, pgid, err)
			}
			t.Logf("agent PID %d, guest PID %d, guest parent PID %d", agent.cmd.Process.Pid, pid, parent)

			if stage == "startup" {
				waitFixtureState(t, pid, true)
				if _, err := os.Stat(filepath.Join(socketDir, "g.sock")); !os.IsNotExist(err) {
					t.Fatalf("startup fixture opened a readiness socket: %v", err)
				}
				if res := cp.CommandResult(100*time.Millisecond, createID); res != nil {
					t.Fatalf("create finished before the guest became ready: %v", res)
				}
			} else {
				res := cp.CommandResult(30*time.Second, createID)
				if res.GetError() != nil || res.GetCreateSandbox().GetSandbox().GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
					t.Fatalf("create result %v\n%s", res, agent.logs.String())
				}
				if !strings.Contains(agent.logs.String(), `"pid":`+strconv.Itoa(pid)) {
					t.Fatalf("sandbox log does not report guest PID %d\n%s", pid, agent.logs.String())
				}
			}

			if stage == "paused" || stage == "delete" {
				runBinarySandboxCommand(t, cp, &ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_PauseSandbox{
					PauseSandbox: &ownsandv1.PauseSandboxCommand{SandboxId: id},
				}}, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED)
				waitFixtureState(t, pid, true)
			}
			if stage == "delete" {
				runBinarySandboxCommand(t, cp, &ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_ResumeSandbox{
					ResumeSandbox: &ownsandv1.ResumeSandboxCommand{SandboxId: id},
				}}, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
				waitFixtureState(t, pid, false)
				runBinarySandboxCommand(t, cp, &ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_DeleteSandbox{
					DeleteSandbox: &ownsandv1.DeleteSandboxCommand{SandboxId: id},
				}}, ownsandv1.SandboxState_SANDBOX_STATE_DELETED)
				waitFixtureReaped(t, time.Now().Add(binaryAgentTimeout), pid, parent)
				requireProcessRuntimePathsRemoved(t, sandbox, socketDir)
				stopBinaryAgent(t, agent)
				return
			}

			if err := agent.cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			deadline = time.Now().Add(binaryAgentTimeout)
			var exitErr *exec.ExitError
			if err := agent.wait(t, deadline); !errors.As(err, &exitErr) || exitErr.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatalf("agent exit after SIGKILL: %v", err)
			}
			waitFixtureReaped(t, deadline, pid, parent)
			if parent == agent.cmd.Process.Pid {
				t.Fatal("the guest did not have a separate supervisor")
			}
			t.Log("guest and supervisor reaped and agent stderr drained before restart")
			for _, path := range []string{sandbox, socketDir} {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("expected lost sandbox path %s before restart: %v", path, err)
				}
			}
			from := cp.Received()
			restarted := startProcessRuntimeAgent(t, bin, env)
			report := restarted.waitForReport(t, cp, from)
			requireProcessRuntimePathsRemoved(t, sandbox, socketDir)
			if !slices.ContainsFunc(report.GetSandboxes(), func(s *ownsandv1.SandboxStateEntry) bool {
				return s.GetSandboxId() == id && s.GetState() == ownsandv1.SandboxState_SANDBOX_STATE_FAILED &&
					s.GetEndReason() == ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE
			}) {
				t.Fatalf("restart did not report sandbox %s as lost: %v", id, report)
			}
			stopBinaryAgent(t, restarted)
		})
	}
}

func runBinarySandboxCommand(t *testing.T, cp *testcp.Server, cmd *ownsandv1.AgentCommand, want ownsandv1.SandboxState) {
	t.Helper()
	cmd.CommandId, _ = uuidv7.NewString()
	if !cp.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Command{Command: cmd}}) {
		t.Fatal("send lifecycle command: agent channel is closed")
	}
	res := cp.CommandResult(binaryAgentTimeout, cmd.CommandId)
	if res.GetError() != nil || res.GetSandbox().GetSandbox().GetState() != want {
		t.Fatalf("%T result = %v; want %s", cmd.GetCommand(), res, want)
	}
}

func processField(pid int, field string) string {
	out, _ := exec.Command("/bin/ps", "-ww", "-o", field+"=", "-p", strconv.Itoa(pid)).Output()
	return strings.TrimSpace(string(out))
}

func waitFixtureState(t *testing.T, pid int, stopped bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var state string
	for time.Now().Before(deadline) {
		state = processField(pid, "stat")
		if state != "" && !strings.HasPrefix(state, "Z") && strings.HasPrefix(state, "T") == stopped {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("fixture helper %d state %q; want stopped=%t", pid, state, stopped)
}

func waitFixtureReaped(t *testing.T, deadline time.Time, pids ...int) {
	t.Helper()
	for _, pid := range pids {
		for {
			err := syscall.Kill(pid, 0)
			if errors.Is(err, syscall.ESRCH) {
				break
			}
			if err != nil || !time.Now().Before(deadline) {
				t.Fatalf("fixture process %d was not reaped: state %q, probe %v", pid, processField(pid, "stat"), err)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}
