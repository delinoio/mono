// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/tide"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
)

func sessionRow(s *tidev1.DevSession) []string {
	return []string{s.GetDevSessionId(), s.GetRunnerProfileName(), enumName(s.GetGuestOs()), enumName(s.GetState()), s.GetLabel(), ts(s.GetQueuedAt())}
}

var sessionHeaders = []string{"id", "profile", "os", "state", "label", "queued"}

func (a *app) sessionProfiles(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	items, err := tide.Collect(c.AllDevSessionProfiles(ctx, &tidev1.ListDevSessionProfilesRequest{OrganizationId: org, PageSize: 200}))
	if err != nil {
		return err
	}
	rows := make([][]string, 0, len(items))
	for _, p := range items {
		r := p.GetResources()
		rows = append(rows, []string{p.GetName(), enumName(p.GetGuestOs()), fmt.Sprintf("%d vCPU, %d MiB, %d GiB", r.GetVcpuCount(), r.GetMemoryMib(), r.GetDiskGib()),
			p.GetWorkspacePath(), dur(p.GetIdleTtl()), dur(p.GetMaxTtl()), dur(p.GetMaxQueueWait())})
	}
	return a.printList(&tidev1.ListDevSessionProfilesResponse{Profiles: items}, []string{"name", "os", "resources", "workspace", "idle ttl", "max ttl", "max queue wait"}, rows)
}

// resolveProfile returns the DEV_SESSION profile with the name.
func resolveProfile(ctx context.Context, c *tide.Client, org, name string) (*tidev1.DevSessionProfile, error) {
	for p, err := range c.AllDevSessionProfiles(ctx, &tidev1.ListDevSessionProfilesRequest{OrganizationId: org, PageSize: 200}) {
		if err != nil {
			return nil, err
		}
		if p.GetName() == name {
			return p, nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("no dev session profile is named %q; 'tide session profiles' lists them", name))
}

func (a *app) sessionCreate(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	label := fs.String("label", "", "label for display (at most 100 characters)")
	wait := fs.String("wait", "", "wait this long until the session is running or ends, for example 10m")
	rest, err := a.positional(fs, args, "profile-name")
	if err != nil {
		return err
	}
	d, err := parseDuration("wait", *wait)
	if err != nil {
		return err
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	prof, err := resolveProfile(ctx, c, org, rest[0])
	if err != nil {
		return err
	}
	res, err := c.DevSessions().CreateDevSession(ctx, connect.NewRequest(&tidev1.CreateDevSessionRequest{OrganizationId: org, RunnerProfileId: prof.GetRunnerProfileId(), Label: *label}))
	if err != nil {
		return err
	}
	s := res.Msg.GetDevSession()
	if d == 0 {
		return a.printMessage(s)
	}
	return a.waitAndPrint(ctx, c, org, s.GetDevSessionId(), d, s)
}

// waitAndPrint reports the outcome of a dev session wait. For create, it
// prints the latest session even on failure and falls back to the created
// session if polling did not read one.
func (a *app) waitAndPrint(ctx context.Context, c *tide.Client, org, id string, d time.Duration, created *tidev1.DevSession) error {
	wctx := ctx
	if d > 0 {
		var cancel context.CancelFunc
		wctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	s, err := c.WaitDevSession(wctx, tide.WaitDevSessionParams{OrganizationID: org, DevSessionID: id})
	if s == nil {
		s = created
	}
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil && s != nil {
		err = nil
	}
	if err != nil && created == nil {
		return err
	}
	printErr := a.printMessage(s)
	if err != nil {
		return err
	}
	if printErr != nil {
		return printErr
	}
	switch {
	case s.GetState() == tidev1.RequestState_REQUEST_STATE_RUNNING:
		return nil
	case tide.IsTerminalState(s.GetState()):
		return &exitError{code: ExitError, msg: fmt.Sprintf("the dev session ended: %s (%s)", enumName(s.GetState()), enumName(s.GetEndReason()))}
	}
	return &exitError{code: ExitError, msg: fmt.Sprintf("the dev session is still %s after %s", enumName(s.GetState()), d)}
}

func (a *app) sessionWait(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	timeout := fs.String("timeout", "", "stop waiting after this long (default: no limit)")
	rest, err := a.positional(fs, args, "session-id")
	if err != nil {
		return err
	}
	d, err := parseDuration("timeout", *timeout)
	if err != nil {
		return err
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	return a.waitAndPrint(ctx, c, org, rest[0], d, nil)
}

func (a *app) sessionList(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	all := fs.Bool("all", false, "list the dev sessions of every member, not only the caller's")
	ended := fs.Bool("include-ended", false, "include ended dev sessions")
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	req := &tidev1.ListDevSessionsRequest{OrganizationId: org, PageSize: 200, IncludeEnded: *ended}
	if !*all {
		req.Owner = &tidev1.ListDevSessionsRequest_OwnedByCaller{OwnedByCaller: true}
	}
	items, err := tide.Collect(c.AllDevSessions(ctx, req))
	if err != nil {
		return err
	}
	rows := make([][]string, 0, len(items))
	for _, s := range items {
		rows = append(rows, sessionRow(s))
	}
	return a.printList(&tidev1.ListDevSessionsResponse{DevSessions: items}, sessionHeaders, rows)
}

func (a *app) sessionGet(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	rest, err := a.positional(fs, args, "session-id")
	if err != nil {
		return err
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := c.DevSessions().GetDevSession(ctx, connect.NewRequest(&tidev1.GetDevSessionRequest{OrganizationId: org, DevSessionId: rest[0]}))
	if err != nil {
		return err
	}
	return a.printMessage(res.Msg.GetDevSession())
}

func (a *app) sessionDelete(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	rest, err := a.positional(fs, args, "session-id")
	if err != nil {
		return err
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := c.DevSessions().DeleteDevSession(ctx, connect.NewRequest(&tidev1.DeleteDevSessionRequest{OrganizationId: org, DevSessionId: rest[0]}))
	if err != nil {
		return err
	}
	return a.printMessage(res.Msg.GetDevSession())
}

// Exec.

func (a *app) sessionExec(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	var envs multiFlag
	fs.Var(&envs, "env", "environment variable KEY=VALUE (repeatable)")
	cwd := fs.String("cwd", "", "working directory: an absolute guest path or a path relative to the workspace")
	timeout := fs.String("timeout", "", "stop the command after this long, for example 10m")
	rest, err := a.parseUntil(fs, args, 2)
	if err != nil {
		return err
	}
	if len(rest) > 1 && rest[1] == "--" {
		rest = append(rest[:1:1], rest[2:]...)
	}
	if len(rest) < 2 {
		return usageErr("usage: tide session exec [flags] <session-id> -- <command> [arguments]")
	}
	env := map[string]string{}
	for _, kv := range envs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return usageErr("--env must be KEY=VALUE")
		}
		env[k] = v
	}
	d, err := parseDuration("timeout", *timeout)
	if err != nil {
		return err
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	w := &execWriter{a: a}
	res, err := c.Exec(ctx, tide.ExecParams{
		OrganizationID: org, DevSessionID: rest[0], Command: rest[1:], Env: env,
		WorkingDirectory: *cwd, Timeout: d, OnEvent: w.event,
	})
	return a.finishExec(ctx, rest[0], w, res, err)
}

func (a *app) sessionAttach(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	so := fs.Int64("stdout-offset", 0, "next stdout byte offset")
	se := fs.Int64("stderr-offset", 0, "next stderr byte offset")
	rest, err := a.positional(fs, args, "session-id", "exec-id")
	if err != nil {
		return err
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	w := &execWriter{a: a}
	res, err := c.AttachExec(ctx, tide.AttachParams{OrganizationID: org, DevSessionID: rest[0], ExecID: rest[1], StdoutOffset: *so, StderrOffset: *se, OnEvent: w.event})
	return a.finishExec(ctx, rest[0], w, res, err)
}

// execWriter writes raw output in text mode and decodes each stream in JSON mode.
type execWriter struct {
	a              *app
	stdout, stderr utf8Stream
}

func (w *execWriter) event(ev tide.ExecEvent) error {
	a := w.a
	if a.fmt == FormatJSON {
		switch ev.Kind {
		case tide.ExecEventStdout, tide.ExecEventStderr:
			stream := w.stream(ev.Kind)
			if ev.Offset != stream.next {
				if err := w.flush(ev.Kind, ev.ExecID); err != nil {
					return err
				}
			}
			offset, data := stream.decode(ev.Offset, ev.Data)
			if data == "" {
				return nil
			}
			return w.writeOutput(ev.Kind, ev.ExecID, offset, data)
		case tide.ExecEventOutputTruncated:
			if !ev.MissingStdout.Empty() {
				if err := w.flush(tide.ExecEventStdout, ev.ExecID); err != nil {
					return err
				}
			}
			if !ev.MissingStderr.Empty() {
				if err := w.flush(tide.ExecEventStderr, ev.ExecID); err != nil {
					return err
				}
			}
			return writeJSON(a.env.Stdout, map[string]any{
				"event": ev.Kind.String(), "exec_id": ev.ExecID,
				"missing_stdout": ev.MissingStdout, "missing_stderr": ev.MissingStderr,
			})
		default:
			return writeJSON(a.env.Stdout, map[string]any{"event": ev.Kind.String(), "exec_id": ev.ExecID})
		}
	}
	switch ev.Kind {
	case tide.ExecEventStdout:
		_, err := a.env.Stdout.Write(ev.Data)
		return err
	case tide.ExecEventStderr:
		_, err := a.env.Stderr.Write(ev.Data)
		return err
	case tide.ExecEventOutputTruncated:
		var parts []string
		if !ev.MissingStdout.Empty() {
			parts = append(parts, fmt.Sprintf("stdout bytes %d to %d", ev.MissingStdout.From, ev.MissingStdout.To))
		}
		if !ev.MissingStderr.Empty() {
			parts = append(parts, fmt.Sprintf("stderr bytes %d to %d", ev.MissingStderr.From, ev.MissingStderr.To))
		}
		a.warnf("output was lost: %s", strings.Join(parts, " and "))
	}
	return nil
}

func (w *execWriter) stream(kind tide.ExecEventKind) *utf8Stream {
	if kind == tide.ExecEventStderr {
		return &w.stderr
	}
	return &w.stdout
}

func (w *execWriter) writeOutput(kind tide.ExecEventKind, execID string, offset int64, data string) error {
	return writeJSON(w.a.env.Stdout, map[string]any{
		"event": kind.String(), "exec_id": execID, "offset": offset, "data": data,
	})
}

func (w *execWriter) flush(kind tide.ExecEventKind, execID string) error {
	stream := w.stream(kind)
	if len(stream.pending) == 0 {
		return nil
	}
	offset := stream.resumeOffset(stream.next)
	stream.pending = stream.pending[:0]
	return w.writeOutput(kind, execID, offset, string(utf8.RuneError))
}

func (w *execWriter) flushAll(execID string) error {
	if err := w.flush(tide.ExecEventStdout, execID); err != nil {
		return err
	}
	return w.flush(tide.ExecEventStderr, execID)
}

// utf8Stream holds up to three bytes of an incomplete UTF-8 character
// until the next contiguous chunk arrives.
type utf8Stream struct {
	pending []byte
	next    int64 // Offset after the last byte received.
}

func (u *utf8Stream) decode(offset int64, data []byte) (int64, string) {
	start := offset - int64(len(u.pending))
	u.next = offset + int64(len(data))
	if len(u.pending) != 0 {
		data = append(u.pending, data...)
	}
	cut := incompleteRuneStart(data)
	text := strings.ToValidUTF8(string(data[:cut]), string(utf8.RuneError))
	u.pending = append(u.pending[:0], data[cut:]...)
	return start, text
}

func (u *utf8Stream) resumeOffset(receivedOffset int64) int64 {
	if len(u.pending) != 0 {
		return u.next - int64(len(u.pending))
	}
	return receivedOffset
}

// incompleteRuneStart returns the start of a valid but incomplete UTF-8
// sequence at the end of data, or len(data) if there is no such sequence.
func incompleteRuneStart(data []byte) int {
	for i := len(data) - 1; i >= 0 && i >= len(data)-utf8.UTFMax+1; i-- {
		if utf8.RuneStart(data[i]) {
			if !utf8.FullRune(data[i:]) {
				return i
			}
			break
		}
	}
	return len(data)
}

// finishExec maps an exec result to the exit status of the ownsand sandbox
// exec rules.
func (a *app) finishExec(ctx context.Context, session string, w *execWriter, res *tide.ExecResult, err error) error {
	jsonMode := a.fmt == FormatJSON
	if err != nil {
		var se *tide.ExecStreamError
		if !errors.As(err, &se) {
			// The server rejected the exec before it ran.
			return err
		}
		if ctx.Err() != nil {
			stdoutOffset, stderrOffset := se.StdoutOffset, se.StderrOffset
			if jsonMode {
				stdoutOffset = w.stdout.resumeOffset(stdoutOffset)
				stderrOffset = w.stderr.resumeOffset(stderrOffset)
			}
			return &exitError{code: ExitInterrupted, msg: fmt.Sprintf("interrupted; the command keeps running. Continue with: tide session attach %s %s --stdout-offset %d --stderr-offset %d", session, se.ExecID, stdoutOffset, stderrOffset)}
		}
		if jsonMode {
			if err := w.flushAll(se.ExecID); err != nil {
				return err
			}
			obj := map[string]any{"event": "stream_error", "exec_id": se.ExecID, "message": describeError(se.Err), "stdout_offset": se.StdoutOffset, "stderr_offset": se.StderrOffset, "outcome_unknown": se.OutcomeUnknown}
			if werr := writeJSON(a.env.Stdout, obj); werr != nil {
				return werr
			}
			return &exitError{code: ExitDataPath}
		}
		return &exitError{code: ExitDataPath, msg: "exec stream: " + describeError(err)}
	}
	a.logger.Debug("tide exec ended", "exec_id", res.ExecID, "result", res.Kind.String())
	if jsonMode {
		if err := w.flushAll(res.ExecID); err != nil {
			return err
		}
		obj := map[string]any{"event": res.Kind.String(), "exec_id": res.ExecID, "incomplete": res.Incomplete,
			"stdout_offset": res.StdoutOffset, "stderr_offset": res.StderrOffset}
		switch res.Kind {
		case tide.ExecResultExited:
			if res.ExitCode != nil {
				obj["exit_code"] = *res.ExitCode
			}
			if res.Signal != nil {
				obj["signal"] = *res.Signal
			}
			obj["timed_out"] = res.TimedOut
		case tide.ExecResultFailed:
			obj["reason"] = enumName(res.FailureReason)
			obj["message"] = res.FailureMessage
		case tide.ExecResultSandboxEnded:
			obj["reason"] = res.SandboxEndReason
		}
		if err := writeJSON(a.env.Stdout, obj); err != nil {
			return err
		}
		if res.Incomplete {
			return &exitError{code: ExitDataPath}
		}
		return nil
	}
	switch res.Kind {
	case tide.ExecResultSandboxEnded:
		a.errorf("the dev session ended before the command finished (%s)", res.SandboxEndReason)
	case tide.ExecResultFailed:
		a.errorf("the command could not start (%s): %s", enumName(res.FailureReason), res.FailureMessage)
	default:
		if res.TimedOut {
			a.errorf("the exec timeout stopped the command")
		}
	}
	if res.Incomplete {
		a.warnf("the output is incomplete")
	}
	return &exitError{code: execExitStatus(res)}
}

// execExitStatus applies the first matching rule: lost output 255, sandbox
// ended 125, command could not start 127, timeout 124, signal 128+N, exit
// code (255 outside 0 to 255).
func execExitStatus(res *tide.ExecResult) int {
	switch {
	case res.Incomplete:
		return ExitDataPath
	case res.Kind == tide.ExecResultSandboxEnded:
		return ExitSandboxEnded
	case res.Kind == tide.ExecResultFailed:
		return ExitCommandNotStart
	case res.TimedOut:
		return ExitExecTimedOut
	case res.Signal != nil:
		return min(ExitSignalBase+int(*res.Signal), 255)
	case res.ExitCode != nil && *res.ExitCode >= 0 && *res.ExitCode <= 255:
		return int(*res.ExitCode)
	}
	return ExitDataPath
}

// Transfers.

func (a *app) progress(prefix string) func(tide.TransferProgress) {
	if a.fmt == FormatJSON {
		return nil
	}
	last := tide.TransferPhase(0)
	return func(p tide.TransferProgress) {
		if p.Phase != last && p.Phase != tide.TransferDone {
			last = p.Phase
			fmt.Fprintf(a.env.Stderr, "%s: %s\n", prefix, p.Phase)
		}
	}
}

func (a *app) sessionSync(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	force := fs.Bool("force", false, "remove a sync lock that a stopped sync left")
	rest, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if len(rest) < 1 || len(rest) > 2 {
		return usageErr("usage: tide session sync [flags] <session-id> [<local-dir>]")
	}
	dir := "."
	if len(rest) == 2 {
		dir = rest[1]
	}
	if !filepath.IsAbs(dir) {
		wd, err := a.env.Getwd()
		if err != nil {
			return err
		}
		dir = filepath.Join(wd, dir)
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := c.Sync(ctx, tide.SyncParams{OrganizationID: org, DevSessionID: rest[0], LocalDir: dir, Force: *force, Progress: a.progress("sync")})
	if err != nil {
		return err
	}
	if a.fmt == FormatJSON {
		return writeJSON(a.env.Stdout, res)
	}
	for _, w := range res.Warnings {
		a.warnf("%s", w)
	}
	for _, s := range res.Submodules {
		a.warnf("skipped the submodule or nested repository %s", s)
	}
	_, err = fmt.Fprintf(a.env.Stdout, "Uploaded %d, deleted %d and skipped %d files; sent %s bytes.\n", res.Uploaded, res.Deleted, res.Skipped, strconv.FormatInt(res.BytesSent, 10))
	return err
}

func (a *app) sessionDownload(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	rest, err := a.positional(fs, args, "session-id", "guest-path", "local-path")
	if err != nil {
		return err
	}
	local := rest[2]
	if !filepath.IsAbs(local) {
		wd, err := a.env.Getwd()
		if err != nil {
			return err
		}
		local = filepath.Join(wd, local)
	}
	org, c, err := a.orgSetup()
	if err != nil {
		return err
	}
	res, err := c.Download(ctx, tide.DownloadParams{OrganizationID: org, DevSessionID: rest[0], GuestPath: rest[1], LocalPath: local, Progress: a.progress("download")})
	if err != nil {
		return err
	}
	if a.fmt == FormatJSON {
		return writeJSON(a.env.Stdout, res)
	}
	for _, w := range res.Warnings {
		a.warnf("%s", w)
	}
	_, err = fmt.Fprintf(a.env.Stdout, "Downloaded %d files (%d bytes) to %s.\n", res.Files, res.Bytes, res.LocalPath)
	return err
}
