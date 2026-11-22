// SPDX-License-Identifier: FSL-1.1-ALv2

package claude

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// HookName is the file name of the spawn-runner hook in the hooks
// directory. cmd/tide-api runs in hook mode when it is started under this
// name (the hooks directory holds a symbolic link to the tide-api binary)
// or with this name as its first argument.
const HookName = "spawn-runner"

// SocketEnv names the hook socket in the orchestrator's environment. The
// Claude Code documentation does not state that the orchestrator passes
// its environment to the hook (NOT DOCUMENTED), so the hook also finds the
// socket next to its hooks directory (socketName).
const SocketEnv = "TIDE_SPAWN_SOCKET"

// socketName is the hook socket in the run directory of an orchestrator,
// the parent of its hooks directory. The name is short because a Unix
// socket path has at most 104 bytes on macOS and 108 on Linux.
const socketName = "sock"

// maxWorkOrderBytes bounds the work order that the hook reads and sends.
const maxWorkOrderBytes = 64 << 10

// maxHookMessage bounds one hook message on the socket.
const maxHookMessage = 256 << 10

// hookDeadline bounds the hook's exchange with tide-api. It is well below
// the orchestrator's --hook-timeout (HookTimeout).
const hookDeadline = 20 * time.Second

// hookRequest is the message of the hook to tide-api: the spawn request
// as the orchestrator gives it to the hook in environment variables.
type hookRequest struct {
	OrderID     string `json:"order_id"`
	SessionID   string `json:"session_id"`
	SessionUUID string `json:"session_uuid,omitempty"`
	Attempt     string `json:"attempt,omitempty"`
	SpawnReason string `json:"spawn_reason,omitempty"`
	// WorkOrder is the single-use work order JWT, a secret.
	WorkOrder string `json:"work_order"`
}

// HookResult is tide-api's answer to a hook.
type HookResult string

const (
	// HookStored: tide stored the request with its work order. Exit 0.
	HookStored HookResult = "stored"
	// HookDuplicate: tide stored the same order ID before. Exit 0, because
	// a redelivery of one order must create at most one workload.
	HookDuplicate HookResult = "duplicate"
	// HookRetry: tide could not store the request now. Exit 1, which the
	// orchestrator treats as retryable.
	HookRetry HookResult = "retry"
	// HookReject: tide can never serve this spawn request (for example, a
	// pre-warming request without a session). Exit 2, which blocks the
	// session until its user sends a message or an Owner retries it.
	HookReject HookResult = "reject"
)

// ExitCode returns the hook's exit code of a result, per the exit-code
// contract of the spawn-runner hook: 0 submitted, 1 retryable, 2 or higher
// non-retryable.
func (r HookResult) ExitCode() int {
	switch r {
	case HookStored, HookDuplicate:
		return 0
	case HookReject:
		return 2
	}
	return 1
}

type hookReply struct {
	Result  HookResult `json:"result"`
	Message string     `json:"message,omitempty"`
}

// SocketPath returns the hook socket of a hook started as arg0: the socket
// in SocketEnv, or the socket in the parent of the hooks directory.
func SocketPath(getenv func(string) string, arg0 string) string {
	if s := getenv(SocketEnv); s != "" {
		return s
	}
	p := arg0
	if abs, err := filepath.Abs(arg0); err == nil {
		p = abs
	}
	return filepath.Join(filepath.Dir(filepath.Dir(p)), socketName)
}

// RunHook is the spawn-runner hook (tide-api in hook mode). It reads the
// spawn request from the orchestrator's environment variables and the
// work order file, sends them to the tide-api process that runs the
// orchestrator over the Unix socket, and returns the exit code of tide's
// answer. It writes a short message without secrets to stderr; the
// orchestrator logs it and shows its last 512 characters as the spawn
// error.
func RunHook(getenv func(string) string, arg0 string, stderr io.Writer) int {
	fail := func(code int, format string, args ...any) int {
		fmt.Fprintf(stderr, "tide spawn-runner: "+format+"\n", args...)
		return code
	}
	req := hookRequest{
		OrderID: getenv("CLAUDE_RUNNER_ORDER_ID"), SessionID: getenv("CLAUDE_RUNNER_SESSION_ID"), SessionUUID: getenv("CLAUDE_RUNNER_SESSION_UUID"),
		Attempt: getenv("CLAUDE_RUNNER_ATTEMPT"), SpawnReason: getenv("CLAUDE_RUNNER_SPAWN_REASON"),
	}
	if req.OrderID == "" {
		return fail(HookReject.ExitCode(), "CLAUDE_RUNNER_ORDER_ID is not set; start this hook only from a Claude Code orchestrator")
	}
	file := getenv("CLAUDE_RUNNER_WORK_ORDER_FILE")
	if file == "" {
		return fail(HookRetry.ExitCode(), "CLAUDE_RUNNER_WORK_ORDER_FILE is not set")
	}
	f, err := os.Open(file)
	if err != nil {
		return fail(HookRetry.ExitCode(), "cannot read the work order file")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxWorkOrderBytes+1))
	_ = f.Close()
	if err != nil || len(data) > maxWorkOrderBytes {
		return fail(HookRetry.ExitCode(), "cannot read the work order file")
	}
	req.WorkOrder = strings.TrimSpace(string(data))
	if req.WorkOrder == "" {
		return fail(HookRetry.ExitCode(), "the work order file is empty")
	}
	reply, err := exchange(SocketPath(getenv, arg0), req)
	if err != nil {
		// tide-api is not reachable (for example, it is restarting):
		// retryable, the session is offered again.
		return fail(HookRetry.ExitCode(), "tide-api did not answer: %v", err)
	}
	msg := reply.Message
	if msg == "" {
		msg = string(reply.Result)
	}
	fmt.Fprintf(stderr, "tide spawn-runner: %s\n", msg)
	return reply.Result.ExitCode()
}

func exchange(socket string, req hookRequest) (*hookReply, error) {
	conn, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		return nil, errors.New("cannot connect to the hook socket")
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(hookDeadline))
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return nil, errors.New("cannot send the spawn request")
	}
	line, err := bufio.NewReaderSize(io.LimitReader(conn, maxHookMessage), 4096).ReadBytes('\n')
	if err != nil {
		return nil, errors.New("no answer on the hook socket")
	}
	var reply hookReply
	if err := json.Unmarshal(line, &reply); err != nil {
		return nil, errors.New("an invalid answer on the hook socket")
	}
	return &reply, nil
}
