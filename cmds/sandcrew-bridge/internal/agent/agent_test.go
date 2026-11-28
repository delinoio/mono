// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/sandcrew-bridge/internal/acp"
)

type countingHandler struct {
	mu      sync.Mutex
	updates int
}

func (h *countingHandler) SessionUpdate(json.RawMessage) {
	h.mu.Lock()
	h.updates++
	h.mu.Unlock()
}

func (h *countingHandler) RequestPermission(context.Context, acp.RequestPermissionRequest) (acp.PermissionOutcome, error) {
	return acp.PermissionOutcome{Outcome: acp.OutcomeCancelled}, nil
}

func (h *countingHandler) ProtocolError(err error) {}

func (h *countingHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.updates
}

const updateLine = `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk"}}}`

// exitedProcess has exited before the bridge read its output.
type exitedProcess struct {
	stdin  *io.PipeWriter
	stdout *io.PipeReader
}

func (p *exitedProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *exitedProcess) Stdout() io.Reader     { return p.stdout }
func (p *exitedProcess) Wait() Exit            { return Exit{Code: 0} }
func (p *exitedProcess) Kill()                 {}

func waitDone(t *testing.T, a *ACPAgent) {
	t.Helper()
	select {
	case <-a.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the agent did not end")
	}
}

func TestDoneWaitsForTheRemainingOutput(t *testing.T) {
	stdout, stdoutWriter := io.Pipe()
	_, stdin := io.Pipe()
	go func() {
		for range 1000 {
			if _, err := stdoutWriter.Write([]byte(updateLine + "\n")); err != nil {
				return
			}
		}
		stdoutWriter.Close()
	}()
	handler := &countingHandler{}
	a := NewACPAgent(&exitedProcess{stdin: stdin, stdout: stdout}, ACPOptions{Handler: handler})
	waitDone(t, a)
	if got := handler.count(); got != 1000 {
		t.Fatalf("handled %d updates before Done, want 1000", got)
	}
}

func TestDoneAfterTheDrainTimeout(t *testing.T) {
	old := drainTimeout
	drainTimeout = 20 * time.Millisecond
	defer func() { drainTimeout = old }()
	// The output never ends, like a child of the adapter that keeps
	// stdout open.
	stdout, stdoutWriter := io.Pipe()
	defer stdoutWriter.Close()
	_, stdin := io.Pipe()
	a := NewACPAgent(&exitedProcess{stdin: stdin, stdout: stdout}, ACPOptions{Handler: &countingHandler{}})
	waitDone(t, a)
}

func TestExecProcessKeepsOutputAfterTheExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	script := fmt.Sprintf(`i=0; while [ $i -lt 1000 ]; do echo '%s'; i=$((i+1)); done`, strings.ReplaceAll(updateLine, "'", ""))
	process, err := ExecProcess("sh", []string{"-c", script}, t.TempDir(), nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	handler := &countingHandler{}
	a := NewACPAgent(process, ACPOptions{Handler: handler})
	waitDone(t, a)
	if got := handler.count(); got != 1000 {
		t.Fatalf("handled %d updates before Done, want 1000", got)
	}
	if exit := a.Exit(); exit.Code != 0 {
		t.Fatalf("exit %+v", exit)
	}
}

// runningProcess runs until Kill.
type runningProcess struct {
	stdin  *io.PipeWriter
	stdout io.Reader
	killed chan struct{}
	once   sync.Once
}

func (p *runningProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *runningProcess) Stdout() io.Reader     { return p.stdout }
func (p *runningProcess) Wait() Exit {
	<-p.killed
	return Exit{Code: -1, Signal: "terminated"}
}
func (p *runningProcess) Kill() { p.once.Do(func() { close(p.killed) }) }

type repeatReader byte

func (r repeatReader) Read(b []byte) (int, error) {
	for i := range b {
		b[i] = byte(r)
	}
	return len(b), nil
}

type errorHandler struct {
	countingHandler
	mu     sync.Mutex
	errors []error
}

func (h *errorHandler) ProtocolError(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.errors = append(h.errors, err)
}

func TestAConnectionFailureStopsTheAgent(t *testing.T) {
	// One message larger than the reader limit.
	stdout := io.MultiReader(strings.NewReader(`{"jsonrpc":"2.0","method":"session/update","params":"`), io.LimitReader(repeatReader('a'), 65<<20))
	_, stdin := io.Pipe()
	process := &runningProcess{stdin: stdin, stdout: stdout, killed: make(chan struct{})}
	handler := &errorHandler{}
	a := NewACPAgent(process, ACPOptions{Handler: handler})
	waitDone(t, a)
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if len(handler.errors) != 1 || !strings.Contains(handler.errors[0].Error(), "ACP connection failed") {
		t.Fatalf("protocol errors %v, want one connection failure", handler.errors)
	}
	if exit := a.Exit(); exit.Signal != "terminated" {
		t.Fatalf("exit %+v", exit)
	}
}
