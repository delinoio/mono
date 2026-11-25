// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	ownsand "github.com/delinoio/mono/go/ownsand"
	"github.com/delinoio/mono/go/ownsand/validate"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// multiFlag collects a repeated key=value flag.
type multiFlag struct {
	name string
	m    map[string]string
}

func (f *multiFlag) String() string { return "" }

func (f *multiFlag) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return fmt.Errorf("--%s wants key=value", f.name)
	}
	if f.m == nil {
		f.m = map[string]string{}
	}
	f.m[k] = val
	return nil
}

func sandboxRows(ss []*ownsandv1.Sandbox) [][]string {
	var rows [][]string
	for _, s := range ss {
		state := enumName(s.GetState())
		if s.LastKnownState != nil {
			state += " (" + enumName(s.GetLastKnownState()) + ")"
		}
		if s.EndReason != nil {
			state += " (" + enumName(s.GetEndReason()) + ")"
		}
		r := s.GetResources()
		rows = append(rows, []string{s.GetSandboxId(), state, enumName(s.GetGuestOs()), s.GetDeviceId(),
			fmt.Sprintf("%d vCPU %d MiB %d GiB", r.GetVcpuCount(), r.GetMemoryMib(), r.GetDiskGib()), ts(s.GetCreateTime())})
	}
	return rows
}

func (a *app) sandboxCreate(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	image := fs.String("image", "", "base image: linux, macos or windows")
	snapshot := fs.String("snapshot", "", "source snapshot ID")
	template := fs.String("template", "", "source template ID")
	idle := fs.String("idle-ttl", "", "idle TTL, for example 30m (required)")
	maxTTL := fs.String("max-ttl", "", "maximum TTL, at most 7d (required)")
	vcpu := int32Var(fs, "vcpu", 0, "vCPU count (required)")
	memory := fs.Int64("memory", 0, "memory in MiB (required)")
	disk := fs.Int64("disk", 0, "disk in GiB (required)")
	device := fs.String("device", "", "device ID")
	hostOS := fs.String("os", "", "host OS filter: linux, macos or windows")
	arch := fs.String("arch", "", "architecture filter: amd64 or arm64")
	network := fs.String("network", "", "network mode: outbound (default) or none")
	wait := fs.Bool("wait", true, "wait until the sandbox is running, paused, failed or deleted")
	labels := &multiFlag{name: "label"}
	fs.Var(labels, "label", "label key=value (repeatable; never put secrets in labels)")
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	sources := 0
	p := ownsand.CreateSandboxParams{Labels: labels.m, SnapshotID: *snapshot, TemplateID: *template, DeviceID: *device}
	for _, s := range []string{*image, *snapshot, *template} {
		if s != "" {
			sources++
		}
	}
	if sources != 1 {
		return usageErr("pass exactly one of --image, --snapshot and --template")
	}
	if *image != "" {
		img, err := parseImage(*image)
		if err != nil {
			return err
		}
		p.BaseImage = img
	}
	if *idle == "" || *maxTTL == "" {
		return usageErr("--idle-ttl and --max-ttl are required; there is no default TTL")
	}
	if *vcpu <= 0 || *memory <= 0 || *disk <= 0 {
		return usageErr("--vcpu, --memory (MiB) and --disk (GiB) are required and must be greater than zero")
	}
	var err error
	if p.IdleTTL, err = parseDuration("idle-ttl", *idle); err != nil {
		return err
	}
	if p.MaxTTL, err = parseDuration("max-ttl", *maxTTL); err != nil {
		return err
	}
	p.Resources = ownsand.Resources{VCPUCount: *vcpu, MemoryMiB: *memory, DiskGiB: *disk}
	if *hostOS != "" {
		v, err := parseHostOS(*hostOS)
		if err != nil {
			return err
		}
		p.HostOS = &v
	}
	if *arch != "" {
		v, err := parseArch(*arch)
		if err != nil {
			return err
		}
		p.Arch = &v
	}
	if *network != "" {
		if p.Network, err = parseNetwork(*network); err != nil {
			return err
		}
	}
	org, err := a.orgSetup()
	if err != nil {
		return err
	}
	p.OrganizationID = org
	sb, err := a.client.CreateSandbox(ctx, p)
	if err != nil {
		if validate.IsValidationError(err) {
			return usageErr("%v", err)
		}
		return err
	}
	info := sb.Info()
	if *wait {
		fmt.Fprintf(a.env.Stderr, "Created sandbox %s; waiting until it is ready...\n", sb.ID())
		if info, err = sb.WaitStable(ctx); err != nil {
			return err
		}
	}
	if err := a.printMessage(info); err != nil {
		return err
	}
	if info.GetState() == ownsandv1.SandboxState_SANDBOX_STATE_FAILED {
		return &exitError{code: ExitError, msg: fmt.Sprintf("sandbox %s failed (%s)", sb.ID(), enumName(info.GetEndReason()))}
	}
	return nil
}

func (a *app) sandboxByArg(fs *flag.FlagSet, args []string) (*ownsand.Sandbox, error) {
	rest, err := a.positional(fs, args, "sandbox-id")
	if err != nil {
		return nil, err
	}
	if err := idArg("sandbox-id", rest[0]); err != nil {
		return nil, err
	}
	org, err := a.orgSetup()
	if err != nil {
		return nil, err
	}
	return a.client.Sandbox(org, rest[0])
}

func (a *app) sandboxGet(ctx context.Context, name string, args []string) error {
	s, err := a.sandboxByArg(a.newFlags(name), args)
	if err != nil {
		return err
	}
	sb, err := s.Get(ctx)
	if err != nil {
		return err
	}
	return a.printMessage(sb)
}

func (a *app) sandboxDelete(ctx context.Context, name string, args []string) error {
	s, err := a.sandboxByArg(a.newFlags(name), args)
	if err != nil {
		return err
	}
	sb, err := s.Delete(ctx)
	if err != nil {
		return err
	}
	return a.printMessage(sb)
}

func (a *app) sandboxPause(ctx context.Context, name string, args []string) error {
	s, err := a.sandboxByArg(a.newFlags(name), args)
	if err != nil {
		return err
	}
	sb, err := s.Pause(ctx)
	if err != nil {
		return err
	}
	return a.printMessage(sb)
}

func (a *app) sandboxResume(ctx context.Context, name string, args []string) error {
	s, err := a.sandboxByArg(a.newFlags(name), args)
	if err != nil {
		return err
	}
	sb, err := s.Resume(ctx)
	if err != nil {
		return err
	}
	return a.printMessage(sb)
}

func (a *app) sandboxList(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	page := addPageFlags(fs)
	device := fs.String("device", "", "device filter")
	state := fs.String("state", "", "state filter")
	ended := fs.Bool("include-ended", false, "include deleted and failed sandboxes")
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	org, err := a.orgSetup()
	if err != nil {
		return err
	}
	req := &ownsandv1.ListSandboxesRequest{OrganizationId: org, PageSize: page.size, PageToken: page.token, IncludeEnded: *ended}
	if *device != "" {
		req.DeviceId = device
	}
	if *state != "" {
		st, err := parseSandboxState(*state)
		if err != nil {
			return err
		}
		req.State = &st
	}
	res, err := a.client.Sandboxes().List(ctx, req)
	if err != nil {
		return err
	}
	return a.printList(res, res.GetNextPageToken(), []string{"ID", "STATE", "GUEST", "DEVICE", "RESOURCES", "CREATED"}, sandboxRows(res.GetSandboxes()))
}

// execExitStatus returns the text-mode exit status of an exec, in the
// order of the contract: data path failure or lost output (255), sandbox
// ended (125), could not start (127), timeout (124), signal (128+N, at
// most 255), exit code (0 to 255, otherwise 255).
func execExitStatus(res *ownsand.ExecResult, dpErr error) int {
	switch {
	case dpErr != nil || res == nil || res.Incomplete:
		return ExitDataPath
	case res.Kind == ownsand.ExecResultSandboxEnded:
		return ExitSandboxEnded
	case res.Kind == ownsand.ExecResultFailed:
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

// execJSONExit returns the JSON-mode exit status: 0 after a complete
// result, 255 after a data path error or lost output.
func execJSONExit(res *ownsand.ExecResult, dpErr error) int {
	if dpErr != nil || res == nil || res.Incomplete {
		return ExitDataPath
	}
	return ExitOK
}

type jsonRange struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
}

// dataPathErrorEvent builds the final JSON line of an exec whose data path
// failed. It carries the exec ID that the SDK error holds, so a script can
// correlate the failure with the exec, which may still run in the sandbox.
// A failure that happened before the exec ID existed has no ID, so the line
// omits exec_id.
func dataPathErrorEvent(err error) execJSONEvent {
	ev := execJSONEvent{Event: "data_path_error", Message: describeError(err)}
	var dp *ownsand.DataPathError
	if errors.As(err, &dp) {
		ev.ExecID = dp.OperationID
	}
	return ev
}

// execJSONEvent is one line of `sandbox exec --format json`.
type execJSONEvent struct {
	Event         string     `json:"event"`
	ExecID        string     `json:"exec_id,omitempty"`
	Offset        *int64     `json:"offset,omitempty"`
	Data          *string    `json:"data,omitempty"`
	DataBase64    *string    `json:"data_base64,omitempty"`
	MissingStdout *jsonRange `json:"missing_stdout,omitempty"`
	MissingStderr *jsonRange `json:"missing_stderr,omitempty"`
	ExitCode      *int64     `json:"exit_code,omitempty"`
	Signal        *int32     `json:"signal,omitempty"`
	TimedOut      *bool      `json:"timed_out,omitempty"`
	Reason        string     `json:"reason,omitempty"`
	Message       string     `json:"message,omitempty"`
	Transport     string     `json:"transport,omitempty"`
}

func (a *app) sandboxExec(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	env := &multiFlag{name: "env"}
	fs.Var(env, "env", "environment variable KEY=VALUE (repeatable)")
	cwd := fs.String("cwd", "", "working directory")
	timeout := fs.String("timeout", "", "exec timeout, for example 10m")
	binary := fs.Bool("binary", false, "with --format json, encode output as base64")
	rest, err := a.parseUntil(fs, args, 2)
	if err != nil {
		return err
	}
	if len(rest) > 1 && rest[1] == "--" {
		rest = append(rest[:1:1], rest[2:]...)
	}
	if len(rest) < 2 {
		return usageErr("usage: ownsand sandbox exec [flags] <sandbox-id> -- <command> [arguments]")
	}
	if err := idArg("sandbox-id", rest[0]); err != nil {
		return err
	}
	d, err := parseDuration("timeout", *timeout)
	if err != nil {
		return err
	}
	org, err := a.orgSetup()
	if err != nil {
		return err
	}
	s, err := a.client.Sandbox(org, rest[0])
	if err != nil {
		return err
	}
	defer s.Close()

	jsonMode := a.fmt == FormatJSON
	jw := &execJSONWriter{a: a, binary: *binary}
	onEvent := func(ev ownsand.ExecEvent) error {
		if jsonMode {
			return jw.event(ev)
		}
		switch ev.Kind {
		case ownsand.ExecEventStdout:
			if _, err := a.env.Stdout.Write(ev.Data); err != nil {
				return &localWriteError{err: err}
			}
		case ownsand.ExecEventStderr:
			if _, err := a.env.Stderr.Write(ev.Data); err != nil {
				return &localWriteError{err: err}
			}
		case ownsand.ExecEventOutputTruncated:
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
	res, err := s.Exec(ctx, ownsand.ExecParams{Command: rest[1:], Env: env.m, WorkingDirectory: *cwd, Timeout: d, OnEvent: onEvent})
	if err != nil {
		if validate.IsValidationError(err) {
			return usageErr("%v", err)
		}
		if ctx.Err() != nil {
			return &exitError{code: ExitInterrupted, msg: "interrupted; the command keeps running in the sandbox"}
		}
		var lw *localWriteError
		if errors.As(err, &lw) {
			// The CLI received the guest output. A local write failure uses
			// the ordinary CLI error status.
			return lw.err
		}
		var dp *ownsand.DataPathError
		if !errors.As(err, &dp) {
			// Errors before the data path (for example a missing
			// credential) are ordinary errors, but an exec never exits 0
			// without a result.
			err = &ownsand.DataPathError{Operation: "exec", Err: err}
		}
		if jsonMode {
			if werr := jw.flushAll(); werr != nil {
				return werr
			}
			if werr := writeJSON(a.env.Stdout, dataPathErrorEvent(err)); werr != nil {
				return werr
			}
			return &exitError{code: ExitDataPath}
		}
		return &exitError{code: ExitDataPath, msg: "data path: " + describeError(err)}
	}
	a.logger.Debug("ownsand exec ended", "exec_id", res.ExecID, "result", res.Kind.String(), "transport", res.Transport.String())
	if jsonMode {
		if err := jw.flushAll(); err != nil {
			return err
		}
		if err := writeJSON(a.env.Stdout, a.jsonResult(res)); err != nil {
			return err
		}
		return &exitError{code: execJSONExit(res, nil)}
	}
	switch res.Kind {
	case ownsand.ExecResultSandboxEnded:
		a.errorf("the sandbox ended before the command finished (%s)", enumName(res.EndReason))
	case ownsand.ExecResultFailed:
		a.errorf("the command could not start (%s): %s", enumName(res.FailureReason), res.FailureMessage)
	default:
		if res.TimedOut {
			a.errorf("the exec timeout stopped the command")
		}
	}
	if res.Incomplete {
		a.warnf("the output is incomplete")
	}
	return &exitError{code: execExitStatus(res, nil)}
}

// execJSONWriter writes the exec events of sandbox exec --format json.
type execJSONWriter struct {
	a      *app
	binary bool
	execID string
	// stdout and stderr decode the text of each stream without --binary.
	stdout, stderr utf8Stream
}

func (w *execJSONWriter) stream(k ownsand.ExecEventKind) *utf8Stream {
	if k == ownsand.ExecEventStderr {
		return &w.stderr
	}
	return &w.stdout
}

func (w *execJSONWriter) event(ev ownsand.ExecEvent) error {
	if ev.ExecID != "" {
		w.execID = ev.ExecID
	}
	switch ev.Kind {
	case ownsand.ExecEventStdout, ownsand.ExecEventStderr:
		if w.binary {
			s := base64.StdEncoding.EncodeToString(ev.Data)
			return w.writeOutput(ev.Kind, ev.Offset, nil, &s)
		}
		u := w.stream(ev.Kind)
		if u.started && ev.Offset != u.next {
			// Bytes are missing before this chunk: a held-back partial
			// character cannot be completed.
			if err := w.flush(ev.Kind); err != nil {
				return err
			}
		}
		off, text := u.decode(ev.Offset, ev.Data)
		if text == "" {
			// The chunk holds only the start of a character; the next
			// chunk carries it.
			return nil
		}
		return w.writeOutput(ev.Kind, off, &text, nil)
	case ownsand.ExecEventOutputTruncated:
		if !ev.MissingStdout.Empty() {
			if err := w.flush(ownsand.ExecEventStdout); err != nil {
				return err
			}
		}
		if !ev.MissingStderr.Empty() {
			if err := w.flush(ownsand.ExecEventStderr); err != nil {
				return err
			}
		}
	}
	return writeJSON(w.a.env.Stdout, w.a.jsonEvent(ev))
}

func (w *execJSONWriter) writeOutput(k ownsand.ExecEventKind, off int64, text, b64 *string) error {
	return writeJSON(w.a.env.Stdout, execJSONEvent{Event: k.String(), ExecID: w.execID, Offset: &off, Data: text, DataBase64: b64})
}

// flush writes a held-back incomplete character of one stream as a
// replacement character.
func (w *execJSONWriter) flush(k ownsand.ExecEventKind) error {
	off, text, ok := w.stream(k).flush()
	if !ok {
		return nil
	}
	return w.writeOutput(k, off, &text, nil)
}

// flushAll flushes both streams when the output ends.
func (w *execJSONWriter) flushAll() error {
	if err := w.flush(ownsand.ExecEventStdout); err != nil {
		return err
	}
	return w.flush(ownsand.ExecEventStderr)
}

// utf8Stream decodes one output stream as UTF-8 text. The guest splits
// output into chunks without regard to character boundaries, so a
// multibyte character can span two chunks: an incomplete character at the
// end of a chunk is held back until the next contiguous chunk arrives, and
// it is replaced with U+FFFD only when the stream ends or bytes are missing
// after it. Other invalid bytes are replaced with U+FFFD.
type utf8Stream struct {
	pending []byte
	// next is the offset after the last byte received.
	next    int64
	started bool
}

// decode returns the offset of the first byte that text represents and the
// text of the complete characters in the pending bytes and data.
func (u *utf8Stream) decode(off int64, data []byte) (int64, string) {
	start := off - int64(len(u.pending))
	buf := make([]byte, 0, len(u.pending)+len(data))
	buf = append(append(buf, u.pending...), data...)
	cut := incompleteRuneStart(buf)
	u.pending = append(u.pending[:0], buf[cut:]...)
	u.next, u.started = off+int64(len(data)), true
	return start, strings.ToValidUTF8(string(buf[:cut]), string(utf8.RuneError))
}

// flush returns the held-back bytes as replacement text.
func (u *utf8Stream) flush() (int64, string, bool) {
	if len(u.pending) == 0 {
		return 0, "", false
	}
	off := u.next - int64(len(u.pending))
	text := strings.ToValidUTF8(string(u.pending), string(utf8.RuneError))
	u.pending = u.pending[:0]
	return off, text, true
}

// incompleteRuneStart returns the index of an incomplete but so far valid
// UTF-8 sequence at the end of b, or len(b) when b ends with a complete
// character or with invalid bytes.
func incompleteRuneStart(b []byte) int {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax+1; i-- {
		if utf8.RuneStart(b[i]) {
			if !utf8.FullRune(b[i:]) {
				return i
			}
			break
		}
	}
	return len(b)
}

// jsonEvent converts an exec event other than an output chunk.
func (a *app) jsonEvent(ev ownsand.ExecEvent) execJSONEvent {
	out := execJSONEvent{Event: ev.Kind.String(), ExecID: ev.ExecID}
	switch ev.Kind {
	case ownsand.ExecEventOutputTruncated:
		if !ev.MissingStdout.Empty() {
			out.MissingStdout = &jsonRange{From: ev.MissingStdout.From, To: ev.MissingStdout.To}
		}
		if !ev.MissingStderr.Empty() {
			out.MissingStderr = &jsonRange{From: ev.MissingStderr.From, To: ev.MissingStderr.To}
		}
	}
	return out
}

func (a *app) jsonResult(res *ownsand.ExecResult) execJSONEvent {
	out := execJSONEvent{Event: res.Kind.String(), ExecID: res.ExecID, Transport: enumName(res.Transport)}
	switch res.Kind {
	case ownsand.ExecResultExited:
		out.ExitCode, out.Signal = res.ExitCode, res.Signal
		t := res.TimedOut
		out.TimedOut = &t
	case ownsand.ExecResultFailed:
		out.Reason, out.Message = enumName(res.FailureReason), res.FailureMessage
	case ownsand.ExecResultSandboxEnded:
		out.Reason = enumName(res.EndReason)
	}
	return out
}

// ---- file ----

func (a *app) fileRead(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	to := fs.String("to", "", "write the contents to this local path (default: standard output)")
	rest, err := a.positional(fs, args, "sandbox-id", "guest-path")
	if err != nil {
		return err
	}
	if err := idArg("sandbox-id", rest[0]); err != nil {
		return err
	}
	org, err := a.orgSetup()
	if err != nil {
		return err
	}
	s, err := a.client.Sandbox(org, rest[0])
	if err != nil {
		return err
	}
	defer s.Close()
	var w io.Writer = a.env.Stdout
	var dst *downloadTarget
	if *to != "" {
		if dst, err = openDownloadTarget(ctx, *to); err != nil {
			return err
		}
		w = dst.f
	}
	n, err := s.ReadFile(ctx, rest[1], w)
	if dst != nil {
		err = dst.finish(err)
	}
	if err != nil {
		if validate.IsValidationError(err) {
			return usageErr("%v", err)
		}
		return err
	}
	a.logger.Debug("ownsand file read", "sandbox_id", s.ID(), "size_bytes", n, "transport", s.Transport().String())
	return nil
}

// downloadTarget is the local destination of file read --to. A regular file,
// new or existing, receives the contents through a temporary file in the
// same directory that replaces it only after the read succeeds, so a failed
// download keeps the previous contents. An existing file that is not a
// regular file (a device or a FIFO) cannot be replaced by a rename and is
// written in place.
type downloadTarget struct {
	f *os.File
	// path is the destination with symbolic links resolved, so that the
	// rename replaces the target of a link (or creates a missing target) and
	// keeps the link.
	path string
	// tmp is the temporary file name, empty when f is the destination.
	tmp string
}

func openDownloadTarget(ctx context.Context, to string) (*downloadTarget, error) {
	path := to
	info, err := os.Stat(to)
	switch {
	case err == nil && !info.Mode().IsRegular():
		f, err := openFileOrCancel(ctx, to, os.O_WRONLY)
		if err != nil {
			return nil, err
		}
		return &downloadTarget{f: f, path: to}, nil
	case err == nil:
		if path, err = filepath.EvalSymlinks(to); err != nil {
			return nil, err
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	default:
		// os.Stat also fails for a symbolic link whose target is missing.
		// Follow such a link by hand, so that the rename creates the target
		// and keeps the link.
		if path, err = resolveMissingTarget(to); err != nil {
			return nil, err
		}
	}
	dir, _ := filepath.Split(path)
	for range 100 {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}
		// The staging name does not contain the destination base name, so a
		// destination name at the filesystem limit still has room for it.
		tmp := filepath.Join(dir, ".ownsand-"+hex.EncodeToString(b[:])+".tmp")
		// 0666 before the umask gives a new destination the mode that
		// os.Create would give it.
		f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if info != nil {
			// Keep the permissions of the file that the download replaces.
			if err := f.Chmod(info.Mode().Perm()); err != nil {
				_ = f.Close()
				_ = os.Remove(tmp)
				return nil, err
			}
		}
		return &downloadTarget{f: f, path: path, tmp: tmp}, nil
	}
	return nil, fmt.Errorf("create a temporary file beside %s: too many name collisions", to)
}

// openFileOrCancel opens an existing path for reading or writing. A FIFO open
// blocks until the other end opens. Cancellation ends the caller's wait, but
// open(2) stays blocked until the other end opens or the process exits. If
// the open returns after cancellation, the helper closes the descriptor.
func openFileOrCancel(ctx context.Context, path string, flag int) (*os.File, error) {
	type opened struct {
		f   *os.File
		err error
	}
	// An unbuffered send transfers ownership only when the caller receives it.
	ch := make(chan opened)
	go func() {
		f, err := os.OpenFile(path, flag, 0)
		select {
		case ch <- opened{f, err}:
		case <-ctx.Done():
			if f != nil {
				_ = f.Close()
			}
		}
	}()
	select {
	case o := <-ch:
		if err := ctx.Err(); err != nil {
			if o.f != nil {
				_ = o.f.Close()
			}
			return nil, err
		}
		return o.f, o.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// maxSymlinkHops bounds the links that resolveMissingTarget follows. It is
// the same limit that Linux uses for a path lookup (40) rounded up, so that
// a link loop fails with an error and not with an endless loop.
const maxSymlinkHops = 255

// resolveMissingTarget returns the path that a download to to must create,
// when os.Stat reported that to does not exist. For a plain missing path that
// is to itself. When to is a symbolic link whose target is missing (a dangling
// link, possibly through a chain of links), it is the path of the missing
// target, so that the download creates the target and keeps every link.
//
// The links are resolved as Linux and macOS resolve them, on every target
// including Windows: a link is resolved before a ".." that follows it, and a
// directory of a link target must exist. When a directory of a link target
// does not exist, the error wraps os.ErrNotExist, so the download fails
// before it creates a file or touches a link. A to whose own directory does
// not exist is returned as is: the user typed that path, so it keeps the
// path rules of the platform, and the caller reports the failure when it
// creates the temporary file.
func resolveMissingTarget(to string) (string, error) {
	path := to
	for hop := range maxSymlinkHops {
		// Resolve the symbolic links in the directory part first. A relative
		// link target that contains ".." is relative to the real directory of
		// the link. Joining it with the lexical directory would collapse ".."
		// before a symlinked directory (alias -> real/sub) is resolved and pick
		// the wrong destination. filepath.Split keeps the directory part
		// uncleaned, so that EvalSymlinks sees any ".." that an absolute link
		// target contains and resolves it as the kernel does.
		if dir, base := filepath.Split(path); dir != "" {
			realDir, err := filepath.EvalSymlinks(dir)
			if errors.Is(err, os.ErrNotExist) {
				if hop == 0 {
					// The directory of to is missing: the caller reports the
					// failure when it creates the temporary file.
					return path, nil
				}
				// A directory of a link target is missing. Fail here and do
				// not leave the failure to the file system calls of the
				// caller: Windows path handling collapses "missing\.."
				// lexically, so there the temporary file and the rename
				// would succeed and create a file beside the link.
				return "", fmt.Errorf("resolve %s: a directory of the link target %s does not exist: %w", to, path, err)
			}
			if err != nil {
				return "", err
			}
			path = filepath.Join(realDir, base)
		}
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			// A missing path, or a path that is not a link, ends the chain.
			// The caller reports a later failure when it creates the file.
			return path, nil
		}
		target, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			// path already has real directory components (see above). Prefix
			// the target with that directory without cleaning it: filepath.Join
			// would collapse "x/.." lexically, before the next iteration
			// resolves x (a symbolic link, or a directory that is missing) the
			// way the kernel does.
			dir, _ := filepath.Split(path)
			target = dir + target
		}
		path = target
	}
	return "", fmt.Errorf("resolve %s: too many levels of symbolic links", to)
}

// finish closes the target and, when readErr is nil, puts the contents in
// place. On an error it removes the temporary file and leaves the
// destination as it was.
func (d *downloadTarget) finish(readErr error) error {
	err := readErr
	if cerr := d.f.Close(); err == nil {
		err = cerr
	}
	if d.tmp == "" {
		return err
	}
	if err == nil {
		err = os.Rename(d.tmp, d.path)
	}
	if err != nil {
		_ = os.Remove(d.tmp)
	}
	return err
}

func (a *app) fileWrite(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	from := fs.String("from", "", "read the contents from this local path (default: standard input)")
	mode := fs.String("mode", "0644", "file mode in octal")
	rest, err := a.positional(fs, args, "sandbox-id", "guest-path")
	if err != nil {
		return err
	}
	if err := idArg("sandbox-id", rest[0]); err != nil {
		return err
	}
	m, err := strconv.ParseUint(*mode, 8, 32)
	// The SDK treats a zero mode as "use 0644", so --mode 0 would silently
	// write a 0644 file; reject it instead of ignoring the requested mode.
	if err != nil || m == 0 || m > 0o7777 {
		return usageErr("--mode must be a nonzero octal file mode up to 07777 such as 0644")
	}
	org, err := a.orgSetup()
	if err != nil {
		return err
	}
	s, err := a.client.Sandbox(org, rest[0])
	if err != nil {
		return err
	}
	defer s.Close()
	// Standard input can block without data, for example on a terminal, and a
	// blocked read cannot observe ctx: read it through a reader that returns
	// when an interrupt cancels ctx.
	var r io.Reader = newCtxReader(ctx, a.env.Stdin)
	if *from != "" {
		f, err := openFileOrCancel(ctx, *from, os.O_RDONLY)
		if err != nil {
			return err
		}
		defer f.Close()
		r, err = uploadFileReader(ctx, f)
		if err != nil {
			return err
		}
	}
	n, err := s.WriteFile(ctx, rest[1], r, ownsand.WriteOptions{Mode: uint32(m)})
	if err != nil {
		if validate.IsValidationError(err) {
			return usageErr("%v", err)
		}
		if ctx.Err() != nil {
			return &exitError{code: ExitInterrupted, msg: "interrupted; the write was not finished"}
		}
		return err
	}
	if a.fmt == FormatJSON {
		return writeJSON(a.env.Stdout, map[string]any{"sandbox_id": s.ID(), "size_bytes": n})
	}
	fmt.Fprintf(a.env.Stderr, "Wrote %d bytes.\n", n)
	return nil
}

// localWriteError identifies a failed local write of guest stdout or stderr.
type localWriteError struct{ err error }

func (e *localWriteError) Error() string { return e.err.Error() }
func (e *localWriteError) Unwrap() error { return e.err }
