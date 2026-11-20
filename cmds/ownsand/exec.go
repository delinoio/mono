package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"strings"
	"time"

	ownsand "github.com/delinoio/mono/packages/ownsand-sdk-go"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Exit statuses of sandbox exec in text mode.
const (
	execExitDataPath     = 255
	execExitSandboxEnded = 125
	execExitNotStarted   = 127
	execExitTimedOut     = 124
	execExitSignalBase   = 128
)

func execCommand() command {
	var (
		env     = kvFlag{}
		cwd     string
		timeout string
		binary  bool
	)
	return command{
		usage: "<sandbox-id> [--env K=V] [--cwd DIR] [--timeout D] [--binary] -- <program> [args...]",
		flags: func(fs *flag.FlagSet) {
			fs.Var(env, "env", "environment variable K=V (repeatable)")
			fs.StringVar(&cwd, "cwd", "", "working directory in the guest")
			fs.StringVar(&timeout, "timeout", "", "stop the command after this duration")
			fs.BoolVar(&binary, "binary", false, "with --format json, encode output chunks in base64")
		},
		run: func(a *app, fs *flag.FlagSet, args []string) error {
			if len(args) < 2 {
				return usagef("want <sandbox-id> -- <program> [args...]")
			}
			o := ownsand.ExecOptions{Command: args[1:], Env: env, WorkingDirectory: cwd}
			if timeout != "" {
				d, err := parseDuration(timeout)
				if err != nil || d <= 0 {
					return usagef("invalid --timeout %q", timeout)
				}
				o.Timeout = d
			}
			sb, err := a.sandbox(args[0])
			if err != nil {
				return err
			}
			defer sb.Close()
			return &exitCodeError{code: a.runExec(sb, o, binary)}
		},
	}
}

// execOutcome is what sandbox exec saw; it selects the exit status.
type execOutcome struct {
	dataPathErr error
	lost        bool
	result      *ownsand.ExecResult
}

// textExitStatus applies the text-mode order: data path failure or lost
// output, sandbox ended, could not start, timeout, signal, exit code.
func textExitStatus(o execOutcome) int {
	if o.dataPathErr != nil || o.lost || o.result == nil {
		return execExitDataPath
	}
	r := o.result
	switch r.Outcome {
	case ownsand.ExecSandboxEnded:
		return execExitSandboxEnded
	case ownsand.ExecFailed:
		return execExitNotStarted
	}
	if r.TimedOut {
		return execExitTimedOut
	}
	if r.Signal != nil {
		code := execExitSignalBase + int(*r.Signal)
		if *r.Signal < 0 || code > 255 {
			return 255
		}
		return code
	}
	if r.ExitCode != nil && *r.ExitCode >= 0 && *r.ExitCode <= 255 {
		return int(*r.ExitCode)
	}
	return 255
}

// jsonExitStatus: 0 when the CLI received the complete result, otherwise 255.
func jsonExitStatus(o execOutcome) int {
	if o.dataPathErr != nil || o.lost || o.result == nil {
		return execExitDataPath
	}
	return 0
}

func streamName(s ownsandv1.OutputStream) string {
	if s == ownsandv1.OutputStream_OUTPUT_STREAM_STDERR {
		return "stderr"
	}
	return "stdout"
}

func (a *app) runExec(sb *ownsand.Sandbox, o ownsand.ExecOptions, binary bool) int {
	start := time.Now()
	st, err := sb.Exec(a.ctx, o)
	if err != nil {
		if a.fmt == formatJSON {
			a.jsonEvent(map[string]any{"event": "data_path_error", "code": ownsand.CodeOf(err).String(), "message": err.Error()})
		} else {
			a.errorf("%v", err)
		}
		if errors.Is(err, ownsand.ErrInvalidArgument) {
			return exitUsage
		}
		return execExitDataPath
	}
	a.log.Debug("exec started", "sandbox_id", sb.ID(), "exec_id", st.ExecID())
	var out execOutcome
	for {
		ev, err := st.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			out.dataPathErr = err
			break
		}
		a.execEvent(ev, &out, st.ExecID(), binary)
	}
	if out.dataPathErr == nil {
		out.result, out.dataPathErr = st.Result()
	}
	if out.result != nil && out.result.Incomplete {
		out.lost = true
	}
	a.log.Debug("exec ended", "sandbox_id", sb.ID(), "exec_id", st.ExecID(), "transport", sb.Transport().String(), "duration", time.Since(start))
	if out.dataPathErr != nil {
		if a.fmt == formatJSON {
			a.jsonEvent(map[string]any{"event": "data_path_error", "code": ownsand.CodeOf(out.dataPathErr).String(), "message": out.dataPathErr.Error()})
		} else {
			a.errorf("data path failure: %v", out.dataPathErr)
		}
	}
	if a.fmt == formatJSON {
		return jsonExitStatus(out)
	}
	return textExitStatus(out)
}

func (a *app) execEvent(ev *ownsand.ExecEvent, out *execOutcome, execID string, binary bool) {
	if a.fmt == formatJSON {
		m := map[string]any{"event": ev.Kind.String()}
		switch ev.Kind {
		case ownsand.ExecEventStarted:
			m["exec_id"] = execID
		case ownsand.ExecEventStdout, ownsand.ExecEventStderr:
			m["offset"] = ev.Offset
			if binary {
				m["data"] = base64.StdEncoding.EncodeToString(ev.Data)
				m["encoding"] = "base64"
			} else {
				m["data"] = strings.ToValidUTF8(string(ev.Data), "�")
			}
		case ownsand.ExecEventOutputTruncated:
			m["gaps"] = gapsJSON(ev.Gaps)
			out.lost = true
		case ownsand.ExecEventExited:
			if ev.ExitCode != nil {
				m["exit_code"] = *ev.ExitCode
			} else if ev.Signal != nil {
				m["signal"] = *ev.Signal
			}
			m["timed_out"] = ev.TimedOut
		case ownsand.ExecEventFailed:
			m["reason"] = ev.Reason
		case ownsand.ExecEventSandboxEnded:
			m["reason"] = strings.TrimPrefix(ev.EndReason.String(), "SANDBOX_END_REASON_")
		}
		a.jsonEvent(m)
		return
	}
	switch ev.Kind {
	case ownsand.ExecEventStdout:
		_, _ = a.io.out.Write(ev.Data)
	case ownsand.ExecEventStderr:
		_, _ = a.io.err.Write(ev.Data)
	case ownsand.ExecEventOutputTruncated:
		out.lost = true
		for _, g := range ev.Gaps {
			a.warnf("lost %s output at offsets [%d, %d)", streamName(g.Stream), g.From, g.To)
		}
	case ownsand.ExecEventFailed:
		a.errorf("the command could not start: %s", ev.Reason)
	case ownsand.ExecEventSandboxEnded:
		a.errorf("the sandbox ended before the command result: %s", strings.TrimPrefix(ev.EndReason.String(), "SANDBOX_END_REASON_"))
	case ownsand.ExecEventExited:
		if ev.TimedOut {
			a.warnf("the exec timeout stopped the command")
		}
	}
}

func gapsJSON(gaps []ownsand.OutputGap) []map[string]any {
	out := make([]map[string]any, 0, len(gaps))
	for _, g := range gaps {
		out = append(out, map[string]any{"stream": streamName(g.Stream), "from": g.From, "to": g.To})
	}
	return out
}

func (a *app) jsonEvent(m map[string]any) {
	_ = json.NewEncoder(a.io.out).Encode(m)
}
