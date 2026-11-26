// SPDX-License-Identifier: FSL-1.1-ALv2

// Package claudefake is a fake Claude Code orchestrator for tests: a
// claude.Starter whose processes run in-process. A test makes the fake
// orchestrator send a spawn request, which runs the real spawn-runner hook
// (claude.RunHook) against the hook socket of sandrun-api, as the
// orchestrator runs ${hooks-dir}/spawn-runner. It never starts a binary.
package claudefake

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/delinoio/mono/servers/sandrun-api/internal/agent/claude"
)

// Fake is the fake orchestrator starter.
type Fake struct {
	t   testing.TB
	dir string

	mu       sync.Mutex
	procs    []*Proc
	failNext int
}

// New returns a fake. Work order files go to a test directory.
func New(t testing.TB) *Fake {
	return &Fake{t: t, dir: t.TempDir()}
}

// FailStarts makes the next n starts fail.
func (f *Fake) FailStarts(n int) {
	f.mu.Lock()
	f.failNext = n
	f.mu.Unlock()
}

// Start is the claude.Starter.
func (f *Fake) Start(spec claude.ProcessSpec) (claude.Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext > 0 {
		f.failNext--
		return nil, errors.New("fake start failure")
	}
	p := &Proc{f: f, Spec: spec, done: make(chan error, 1)}
	f.procs = append(f.procs, p)
	return p, nil
}

// Procs returns every started process, oldest first.
func (f *Fake) Procs() []*Proc {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*Proc(nil), f.procs...)
}

// Running returns the processes that did not exit.
func (f *Fake) Running() []*Proc {
	var out []*Proc
	for _, p := range f.Procs() {
		if !p.Exited() {
			out = append(out, p)
		}
	}
	return out
}

// Proc is one fake orchestrator process.
type Proc struct {
	f    *Fake
	Spec claude.ProcessSpec

	mu      sync.Mutex
	exited  bool
	signals []os.Signal
	done    chan error
}

func (p *Proc) exit(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exited {
		return
	}
	p.exited = true
	p.done <- err
}

// Wait blocks until the process exits.
func (p *Proc) Wait() error { return <-p.done }

// Signal records the signal; SIGTERM ends the fake after its (empty)
// drain.
func (p *Proc) Signal(sig os.Signal) error {
	p.mu.Lock()
	p.signals = append(p.signals, sig)
	p.mu.Unlock()
	if sig == syscall.SIGTERM {
		p.exit(nil)
	}
	return nil
}

// Kill ends the process.
func (p *Proc) Kill() error {
	p.exit(errors.New("killed"))
	return nil
}

// Crash ends the process with an error, like a crashed orchestrator.
func (p *Proc) Crash() { p.exit(errors.New("exit status 1")) }

// Exited reports whether the process ended.
func (p *Proc) Exited() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exited
}

// Signals returns the received signals.
func (p *Proc) Signals() []os.Signal {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]os.Signal(nil), p.signals...)
}

// Env returns one variable of the process environment.
func (p *Proc) Env(name string) string {
	for _, kv := range p.Spec.Env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			return v
		}
	}
	return ""
}

// Arg returns the value after a flag of the process arguments.
func (p *Proc) Arg(flag string) string {
	for i, a := range p.Spec.Args {
		if a == flag && i+1 < len(p.Spec.Args) {
			return p.Spec.Args[i+1]
		}
	}
	return ""
}

// Spawn is one spawn request of the orchestrator: the fields of the hook
// environment.
type Spawn struct {
	OrderID, SessionID, Reason string
	WorkOrder                  string
	// NoWorkOrderFile leaves CLAUDE_RUNNER_WORK_ORDER_FILE unset.
	NoWorkOrderFile bool
}

// Spawn runs the spawn-runner hook of the process for one spawn request,
// like the orchestrator: the work order in a temporary file (deleted after
// the hook exits), the CLAUDE_RUNNER_* variables, and the hook path in the
// hooks directory. The hook gets only these variables, not the
// orchestrator environment, because the documentation does not state that
// the orchestrator passes it on. It returns the exit code and stderr.
func (p *Proc) Spawn(s Spawn) (int, string) {
	env := map[string]string{"CLAUDE_RUNNER_ORDER_ID": s.OrderID, "CLAUDE_RUNNER_SESSION_ID": s.SessionID, "CLAUDE_RUNNER_SPAWN_REASON": s.Reason,
		"CLAUDE_RUNNER_ATTEMPT": "1", "CLAUDE_RUNNER_POOL_ID": "ccpool_test", "CLAUDE_RUNNER_ACCOUNT_EMAIL": "dev@example.com"}
	if !s.NoWorkOrderFile {
		f, err := os.CreateTemp(p.f.dir, "work-order-")
		if err != nil {
			p.f.t.Fatal(err)
		}
		_, _ = f.WriteString(s.WorkOrder)
		_ = f.Close()
		defer os.Remove(f.Name())
		env["CLAUDE_RUNNER_WORK_ORDER_FILE"] = f.Name()
	}
	var stderr bytes.Buffer
	code := claude.RunHook(func(k string) string { return env[k] }, filepath.Join(p.Arg("--hooks-dir"), claude.HookName), &stderr)
	return code, stderr.String()
}
