// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/sandboxes"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/testcp"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestProbeProcessRuntimeRestartWithUnreadableSocketRecord(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Fatal("run the probe as an unprivileged user")
	}
	bin := buildProcessRuntimeBinaries(t)
	cp := testcp.New(t)
	cp.SetAck(true, false)
	stateDir := t.TempDir()
	env := processRuntimeEnv(stateDir, filepath.Join(bin, "ownsand-guest"))
	enrollProcessRuntimeAgent(t, bin, cp, env)
	agent := startProcessRuntimeAgent(t, bin, env)
	agent.waitForReport(t, cp, 0)
	id, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	cmdID := sendProcessRuntimeCreate(t, cp, id)
	result := cp.CommandResult(30*time.Second, cmdID)
	if result.GetError() != nil || result.GetCreateSandbox().GetSandbox().GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("sandbox creation failed: %v\n%s", result, agent.logs.String())
	}

	sandboxDir := filepath.Join(stateDir, "vms", id)
	socketRecord := filepath.Join(sandboxDir, "socket-dir")
	persistedPath := filepath.Join(stateDir, "sandboxes", id+".json")
	var socketDir string
	if runtime.GOOS == "darwin" {
		socketDir = processRuntimeSocketDir(t, sandboxDir)
		t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
		if socket, err := os.Stat(filepath.Join(socketDir, "g.sock")); err != nil || socket.Mode()&os.ModeSocket == 0 {
			t.Fatalf("expected a real external guest helper socket: %v, %v", socket, err)
		}
		t.Log("the macOS guest helper opened a real filesystem socket outside the sandbox")
	} else {
		socketDir, err = os.MkdirTemp(t.TempDir(), "osb-")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(socketDir, "g.sock"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(socketRecord, []byte(socketDir), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Log("the Linux guest helper used an abstract socket with a separate filesystem cleanup fixture")
	}
	if err := os.Chmod(socketRecord, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(socketRecord); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("expected permission error from socket record read, got %v", err)
	}
	if err := agent.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	var exitErr *exec.ExitError
	if err := agent.wait(t, time.Now().Add(binaryAgentTimeout)); !errors.As(err, &exitErr) || exitErr.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("agent exit after SIGKILL: %v", err)
	}
	before, err := os.ReadFile(persistedPath)
	if err != nil {
		t.Fatal(err)
	}

	from := cp.Received()
	restarted := startProcessRuntimeAgent(t, bin, env)
	report := restarted.waitForReport(t, cp, from)
	var entry *ownsandv1.SandboxStateEntry
	for _, sand := range report.GetSandboxes() {
		if sand.GetSandboxId() == id {
			entry = sand
			break
		}
	}
	if entry.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING ||
		entry.GetEndReason() != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED {
		t.Fatalf("expected RUNNING without an end reason after denied read, got %v\n%s", entry, restarted.logs.String())
	}
	for _, path := range []string{sandboxDir, socketRecord, socketDir, filepath.Join(socketDir, "g.sock")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("startup did not preserve %s: %v", path, err)
		}
	}
	if data, err := os.ReadFile(persistedPath); err != nil || !bytes.Equal(before, data) {
		t.Fatalf("persistent sandbox record changed after denied read (read error: %v)", err)
	}
	t.Log("first restart preserved all cleanup paths and the persisted RUNNING record, and reported RUNNING")

	stopBinaryAgent(t, restarted)
	if err := os.Chmod(socketRecord, 0o600); err != nil {
		t.Fatal(err)
	}
	from = cp.Received()
	retried := startProcessRuntimeAgent(t, bin, env)
	report = retried.waitForReport(t, cp, from)
	entry = nil
	for _, sand := range report.GetSandboxes() {
		if sand.GetSandboxId() == id {
			entry = sand
			break
		}
	}
	if entry.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_FAILED ||
		entry.GetEndReason() != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE {
		t.Fatalf("expected FAILED/LOST_ON_DEVICE after cleanup retry, got %v\n%s", entry, retried.logs.String())
	}
	requireProcessRuntimePathsRemoved(t, socketDir, sandboxDir)
	data, err := os.ReadFile(persistedPath)
	if err != nil {
		t.Fatal(err)
	}
	var persisted sandboxes.Record
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.State != entry.GetState() || persisted.EndReason != entry.GetEndReason() {
		t.Fatalf("persisted state %v/%v differs from report %v", persisted.State, persisted.EndReason, entry)
	}
	t.Log("second restart removed the socket and sandbox paths and persisted and reported FAILED/LOST_ON_DEVICE")
	stopBinaryAgent(t, retried)
}
