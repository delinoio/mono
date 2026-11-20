// SPDX-License-Identifier: Apache-2.0

package guest

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/cmds/ownsand-guest/execlog"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// KillGrace is the time between SIGTERM and SIGKILL when an exec timeout
// ends on Linux and macOS guests.
const KillGrace = 10 * time.Second

// pipeWaitDelay bounds how long the helper waits for the output pipes after
// the command exits, because a background child may keep them open.
const pipeWaitDelay = 2 * time.Second

type execEntry struct {
	log *execlog.Log
}

// execManager runs commands independently of the stream that started them
// and keeps their output and result for AttachExec.
type execManager struct {
	mu        sync.Mutex
	execs     map[string]*execEntry
	now       func() time.Time
	retention time.Duration
	capacity  int
	killGrace time.Duration
	logger    *slog.Logger
	wg        sync.WaitGroup
}

func newExecManager(now func() time.Time, logger *slog.Logger) *execManager {
	return &execManager{
		execs:     map[string]*execEntry{},
		now:       now,
		retention: execlog.Retention,
		capacity:  execlog.KeptOutputBytes,
		killGrace: KillGrace,
		logger:    logger,
	}
}

// start runs req, or returns the log of the exec with the same exec_id when
// it is known. The device agent guarantees that a repeated exec_id carries
// the same request.
func (m *execManager) start(req *ownsandv1.ExecRequest) (*execlog.Log, bool) {
	m.mu.Lock()
	if e, ok := m.execs[req.GetExecId()]; ok {
		m.mu.Unlock()
		return e.log, false
	}
	l := execlog.New(req.GetExecId(), m.capacity)
	m.execs[req.GetExecId()] = &execEntry{log: l}
	m.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.run(req, l)
	}()
	return l, true
}

func (m *execManager) lookup(execID string) (*execlog.Log, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.execs[execID]
	if !ok {
		return nil, false
	}
	return e.log, true
}

// sweep forgets execs that ended more than the retention time ago.
func (m *execManager) sweep(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, e := range m.execs {
		if end := e.log.EndTime(); !end.IsZero() && now.Sub(end) >= m.retention {
			delete(m.execs, id)
		}
	}
}

type outputWriter func([]byte)

func (w outputWriter) Write(p []byte) (int, error) {
	w(p)
	return len(p), nil
}

func failedEvent(reason ownsandv1.ExecFailureReason, msg string) *ownsandv1.ExecEvent {
	return &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Failed{Failed: &ownsandv1.ExecFailed{Reason: reason, Message: msg}}}
}

// commandEnv returns the helper's own environment with the request
// variables on top, sorted for a stable order.
func commandEnv(env map[string]string) []string {
	merged := map[string]string{}
	for _, kv := range os.Environ() {
		for i := 1; i < len(kv); i++ {
			if kv[i] == '=' {
				merged[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	for k, v := range env {
		merged[k] = v
	}
	out := make([]string, 0, len(merged))
	for k, v := range merged {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

func startFailure(err error) *ownsandv1.ExecEvent {
	switch {
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, fs.ErrNotExist):
		return failedEvent(ownsandv1.ExecFailureReason_EXEC_FAILURE_REASON_PROGRAM_NOT_FOUND, "program not found")
	case errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.ENOEXEC):
		return failedEvent(ownsandv1.ExecFailureReason_EXEC_FAILURE_REASON_PERMISSION_DENIED, "program cannot be executed")
	case errors.Is(err, syscall.EAGAIN), errors.Is(err, syscall.ENOMEM):
		return failedEvent(ownsandv1.ExecFailureReason_EXEC_FAILURE_REASON_RESOURCE_EXHAUSTED, "guest has no resources to start the process")
	}
	return failedEvent(ownsandv1.ExecFailureReason_EXEC_FAILURE_REASON_OTHER, "process could not start")
}

func (m *execManager) run(req *ownsandv1.ExecRequest, l *execlog.Log) {
	logger := m.logger.With("exec_id", req.GetExecId())
	now := m.now()
	if wd := req.GetWorkingDirectory(); wd != "" {
		if st, err := os.Stat(wd); err != nil || !st.IsDir() {
			l.Start(now)
			l.Finish(failedEvent(ownsandv1.ExecFailureReason_EXEC_FAILURE_REASON_WORKING_DIRECTORY_INVALID, "working directory does not exist or is not a directory"), m.now())
			logger.Info("exec failed to start", "reason", "working_directory_invalid")
			return
		}
	}
	cmd := exec.Command(req.GetCommand()[0], req.GetCommand()[1:]...)
	cmd.Dir = req.GetWorkingDirectory()
	cmd.Env = commandEnv(req.GetEnv())
	cmd.Stdout = outputWriter(l.WriteStdout)
	cmd.Stderr = outputWriter(l.WriteStderr)
	cmd.WaitDelay = pipeWaitDelay
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		ev := startFailure(err)
		l.Start(now)
		l.Finish(ev, m.now())
		logger.Info("exec failed to start", "reason", ev.GetFailed().GetReason().String())
		return
	}
	l.Start(now)
	logger.Info("exec started", "pid", cmd.Process.Pid)

	var timedOut atomic.Bool
	var timers []*time.Timer
	var timersMu sync.Mutex
	m.mu.Lock()
	grace := m.killGrace
	m.mu.Unlock()
	if timeout := req.GetTimeout(); timeout != nil && timeout.AsDuration() > 0 {
		timersMu.Lock()
		timers = append(timers, time.AfterFunc(timeout.AsDuration(), func() {
			timedOut.Store(true)
			logger.Info("exec timeout reached; terminating")
			terminate(cmd.Process)
			if forceAfterTerminate {
				timersMu.Lock()
				timers = append(timers, time.AfterFunc(grace, func() {
					logger.Info("exec did not stop after SIGTERM; killing")
					kill(cmd.Process)
				}))
				timersMu.Unlock()
			}
		}))
		timersMu.Unlock()
	}
	waitErr := cmd.Wait()
	timersMu.Lock()
	for _, t := range timers {
		t.Stop()
	}
	timersMu.Unlock()
	exited := &ownsandv1.ExecExited{TimedOut: timedOut.Load(), EndTime: timestamppb.New(m.now())}
	if cmd.ProcessState == nil {
		logger.Warn("exec wait failed", "error", waitErr)
		l.Finish(failedEvent(ownsandv1.ExecFailureReason_EXEC_FAILURE_REASON_OTHER, "process state unavailable"), m.now())
		return
	}
	code, signal, signaled := exitStatus(cmd.ProcessState)
	if signaled {
		exited.Status = &ownsandv1.ExecExited_Signal{Signal: signal}
	} else {
		exited.Status = &ownsandv1.ExecExited_ExitCode{ExitCode: code}
	}
	l.Finish(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Exited{Exited: exited}}, m.now())
	logger.Info("exec exited", "exit_code", code, "signal", signal, "timed_out", exited.GetTimedOut())
}
