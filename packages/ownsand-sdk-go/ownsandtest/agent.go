package ownsandtest

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"strings"
	"sync"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Program is a fake command. It runs in its own goroutine, independent of
// any stream, and ends the exec by returning.
type Program func(ctx context.Context, x *ExecIO)

// ExecIO is the output and result of one fake command.
type ExecIO struct {
	rec *execRec
	// Request is the exec request (sandbox content of the test).
	Request *ownsandv1.ExecRequest
	// Server is the fake server, for its clock.
	Server *Server
}

// Stdout appends to stdout.
func (x *ExecIO) Stdout(b []byte) { x.rec.write(ownsandv1.OutputStream_OUTPUT_STREAM_STDOUT, b) }

// Stderr appends to stderr.
func (x *ExecIO) Stderr(b []byte) { x.rec.write(ownsandv1.OutputStream_OUTPUT_STREAM_STDERR, b) }

// Agent is the fake device agent. Its exec, read and write state follows the
// protos contract: an exec runs at most once per exec ID, keeps the last
// KeepBytes of each output stream, and file operations are bound to their
// request digests.
type Agent struct {
	s  *Server
	mu sync.Mutex
	// Programs maps a command name (Command[0]) to its fake program. An
	// unknown command fails to start.
	Programs map[string]Program
	// KeepBytes is the kept output per stream (default 8 MiB).
	KeepBytes int
	// ReadChunk is the file chunk size (default 1 MiB).
	ReadChunk int
	files     map[string][]byte
	execs     map[string]*execRec
	runs      map[string]int
	reads     map[string]*readRec
	writes    map[string]*writeRec
}

func newAgent(s *Server) *Agent {
	a := &Agent{s: s, KeepBytes: 8 << 20, ReadChunk: datapath.MaxFileChunk, files: map[string][]byte{}, execs: map[string]*execRec{},
		runs: map[string]int{}, reads: map[string]*readRec{}, writes: map[string]*writeRec{}}
	a.Programs = map[string]Program{
		// echo prints its arguments and exits with 0.
		"echo": func(ctx context.Context, x *ExecIO) {
			x.Stdout([]byte(strings.Join(x.Request.GetCommand()[1:], " ") + "\n"))
			x.Exit(0)
		},
	}
	return a
}

// SetFile sets the content of a guest file.
func (a *Agent) SetFile(path string, data []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.files[path] = bytes.Clone(data)
}

// File returns the content of a guest file.
func (a *Agent) File(path string) ([]byte, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	b, ok := a.files[path]
	return bytes.Clone(b), ok
}

// Runs returns how often the command of execID ran.
func (a *Agent) Runs(execID string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.runs[execID]
}

// TotalRuns returns how many commands ran.
func (a *Agent) TotalRuns() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, v := range a.runs {
		n += v
	}
	return n
}

// ExecIDs returns the known exec IDs.
func (a *Agent) ExecIDs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var ids []string
	for id := range a.execs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (a *Agent) wakeAll() {
	a.mu.Lock()
	recs := make([]*execRec, 0, len(a.execs))
	for _, r := range a.execs {
		recs = append(recs, r)
	}
	a.mu.Unlock()
	for _, r := range recs {
		r.wake()
	}
}

type outBuf struct {
	base int64
	data []byte
}

type execRec struct {
	a      *Agent
	req    *ownsandv1.ExecRequest
	digest []byte
	mu     sync.Mutex
	out    [3]outBuf
	result *ownsandv1.ExecEvent
	notify chan struct{}
}

func (r *execRec) wake() {
	r.mu.Lock()
	close(r.notify)
	r.notify = make(chan struct{})
	r.mu.Unlock()
}

func (r *execRec) write(s ownsandv1.OutputStream, b []byte) {
	r.mu.Lock()
	o := &r.out[s]
	o.data = append(o.data, b...)
	if extra := len(o.data) - r.a.KeepBytes; extra > 0 {
		o.data = append([]byte(nil), o.data[extra:]...)
		o.base += int64(extra)
	}
	r.mu.Unlock()
	r.wake()
}

func (r *execRec) finish(ev *ownsandv1.ExecEvent) {
	r.mu.Lock()
	if r.result == nil {
		r.result = ev
	}
	r.mu.Unlock()
	r.wake()
}

// Exit ends the command with an exit code.
func (x *ExecIO) Exit(code int64) {
	x.rec.finish(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Exited_{Exited: &ownsandv1.ExecEvent_Exited{Result: &ownsandv1.ExecEvent_Exited_ExitCode{ExitCode: code}}}})
}

// ExitTimedOut ends the command with an exit code and timed_out.
func (x *ExecIO) ExitTimedOut(code int64) {
	x.rec.finish(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Exited_{Exited: &ownsandv1.ExecEvent_Exited{Result: &ownsandv1.ExecEvent_Exited_ExitCode{ExitCode: code}, TimedOut: true}}})
}

// Signal ends the command with a signal.
func (x *ExecIO) Signal(sig int32) {
	x.rec.finish(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Exited_{Exited: &ownsandv1.ExecEvent_Exited{Result: &ownsandv1.ExecEvent_Exited_Signal{Signal: sig}}}})
}

// Fail reports that the command could not start.
func (x *ExecIO) Fail(reason string) {
	x.rec.finish(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Failed_{Failed: &ownsandv1.ExecEvent_Failed{Reason: reason}}})
}

// SandboxEnded ends the exec with a sandbox end reason.
func (x *ExecIO) SandboxEnded(reason ownsandv1.SandboxEndReason) {
	x.rec.finish(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_SandboxEnded_{SandboxEnded: &ownsandv1.ExecEvent_SandboxEnded{Reason: reason}}})
}

func (a *Agent) requireRunning(sandbox string) error {
	if a.s.state(sandbox) != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("sandbox is not running"))
	}
	return nil
}

func (a *Agent) checkNewID(id string) error {
	u, err := datapath.ParseUUIDv7(id)
	if err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	switch datapath.ClassifyOperationID(u, a.s.Clock.Now()) {
	case datapath.OperationIDExpired:
		return connect.NewError(connect.CodeNotFound, errors.New("the operation expired or the caller's clock may be behind"))
	case datapath.OperationIDFuture:
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("clock skew"))
	}
	return nil
}

// Exec runs or attaches to an exec. accept is called when the agent
// accepts the request.
func (a *Agent) Exec(ctx context.Context, req *ownsandv1.ExecRequest, accept func() error, send func(*ownsandv1.ExecEvent) error) error {
	digest := datapath.ExecDigest(req)
	a.mu.Lock()
	rec := a.execs[req.GetExecId()]
	if rec != nil {
		a.mu.Unlock()
		if !bytes.Equal(rec.digest, digest) {
			return connect.NewError(connect.CodeAlreadyExists, errors.New("exec ID reused with another request"))
		}
		if err := accept(); err != nil {
			return err
		}
		return rec.stream(ctx, 0, 0, send)
	}
	if err := a.requireRunning(req.GetSandboxId()); err != nil {
		a.mu.Unlock()
		return err
	}
	if err := a.checkNewID(req.GetExecId()); err != nil {
		a.mu.Unlock()
		return err
	}
	rec = &execRec{a: a, req: req, digest: digest, notify: make(chan struct{})}
	a.execs[req.GetExecId()] = rec
	a.runs[req.GetExecId()]++
	prog := a.Programs[req.GetCommand()[0]]
	a.mu.Unlock()
	x := &ExecIO{rec: rec, Request: req, Server: a.s}
	if prog == nil {
		x.Fail("executable not found")
	} else {
		go prog(context.Background(), x)
	}
	if err := accept(); err != nil {
		return err
	}
	return rec.stream(ctx, 0, 0, send)
}

// Attach continues the events of an exec from offsets.
func (a *Agent) Attach(ctx context.Context, req *ownsandv1.AttachExecRequest, kind ownsandv1.RelayOperationKind, grantDigest []byte, accept func() error, send func(*ownsandv1.ExecEvent) error) error {
	a.mu.Lock()
	rec := a.execs[req.GetExecId()]
	a.mu.Unlock()
	if rec == nil || rec.req.GetSandboxId() != req.GetSandboxId() {
		return connect.NewError(connect.CodeNotFound, errors.New("exec not found"))
	}
	if kind == ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC && !bytes.Equal(grantDigest, rec.digest) {
		return connect.NewError(connect.CodePermissionDenied, errors.New("grant digest does not match the exec"))
	}
	st := a.s.state(req.GetSandboxId())
	if st != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING && st != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("sandbox is not running or paused"))
	}
	if err := accept(); err != nil {
		return err
	}
	return rec.stream(ctx, req.GetStdoutOffset(), req.GetStderrOffset(), send)
}

// stream sends started, a truncation notice, output from the offsets and
// the result. It ends with paused when the sandbox is paused before the
// result.
func (r *execRec) stream(ctx context.Context, outOff, errOff int64, send func(*ownsandv1.ExecEvent) error) error {
	if err := send(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Started_{Started: &ownsandv1.ExecEvent_Started{ExecId: r.req.GetExecId()}}}); err != nil {
		return err
	}
	off := [3]int64{0, outOff, errOff}
	r.mu.Lock()
	ob, eb := r.out[1].base, r.out[2].base
	r.mu.Unlock()
	if outOff < ob || errOff < eb {
		tr := &ownsandv1.ExecEvent_OutputTruncated{FirstStdoutOffset: max(outOff, ob), FirstStderrOffset: max(errOff, eb)}
		off[1], off[2] = tr.FirstStdoutOffset, tr.FirstStderrOffset
		if err := send(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_OutputTruncated_{OutputTruncated: tr}}); err != nil {
			return err
		}
	}
	for {
		r.mu.Lock()
		notify := r.notify
		var pending []*ownsandv1.ExecEvent
		for _, s := range []ownsandv1.OutputStream{ownsandv1.OutputStream_OUTPUT_STREAM_STDOUT, ownsandv1.OutputStream_OUTPUT_STREAM_STDERR} {
			o := r.out[s]
			if off[s] < o.base {
				off[s] = o.base
			}
			for off[s] < o.base+int64(len(o.data)) {
				start := off[s] - o.base
				end := min(start+datapath.MaxExecOutputChunk, int64(len(o.data)))
				pending = append(pending, &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Output_{Output: &ownsandv1.ExecEvent_Output{
					Stream: s, Offset: off[s], Data: bytes.Clone(o.data[start:end])}}})
				off[s] = o.base + end
			}
		}
		result := r.result
		r.mu.Unlock()
		for _, ev := range pending {
			if err := send(ev); err != nil {
				return err
			}
		}
		if result != nil {
			return send(result)
		}
		if r.a.s.state(r.req.GetSandboxId()) == ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
			return send(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Paused_{Paused: &ownsandv1.ExecEvent_Paused{}}})
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-notify:
		}
	}
}

type readRec struct {
	digest []byte
	data   []byte
}

// Read serves a file read from the snapshot of its read ID.
func (a *Agent) Read(ctx context.Context, req *ownsandv1.ReadFileRequest, accept func() error, send func(*ownsandv1.ReadFileResponse) error) error {
	if err := a.requireRunning(req.GetSandboxId()); err != nil {
		return err
	}
	digest := datapath.ReadDigest(req)
	a.mu.Lock()
	rec := a.reads[req.GetReadId()]
	if rec == nil {
		if err := a.checkNewID(req.GetReadId()); err != nil {
			a.mu.Unlock()
			return err
		}
		data, ok := a.files[req.GetPath()]
		if !ok {
			a.mu.Unlock()
			return connect.NewError(connect.CodeNotFound, errors.New("file not found"))
		}
		rec = &readRec{digest: digest, data: bytes.Clone(data)}
		a.reads[req.GetReadId()] = rec
	} else if !bytes.Equal(rec.digest, digest) {
		a.mu.Unlock()
		return connect.NewError(connect.CodePermissionDenied, errors.New("read digest mismatch"))
	}
	chunk := a.ReadChunk
	a.mu.Unlock()
	if err := accept(); err != nil {
		return err
	}
	for off := req.GetOffset(); off < int64(len(rec.data)); off += int64(chunk) {
		end := min(off+int64(chunk), int64(len(rec.data)))
		if err := send(&ownsandv1.ReadFileResponse{Message: &ownsandv1.ReadFileResponse_Chunk{Chunk: &ownsandv1.FileChunk{Offset: off, Data: rec.data[off:end]}}}); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return send(&ownsandv1.ReadFileResponse{Message: &ownsandv1.ReadFileResponse_Finished_{Finished: &ownsandv1.ReadFileResponse_Finished{SizeBytes: int64(len(rec.data))}}})
}

type writeRec struct {
	digest   []byte
	path     string
	data     []byte
	written  []bool
	finished *ownsandv1.WriteFileFinish
}

// Write applies one WriteFile chunk. checkGrant compares grantDigest with
// the recorded digest (relay only).
func (a *Agent) Write(req *ownsandv1.WriteFileRequest, grantDigest []byte, checkGrant bool) (*ownsandv1.WriteFileResponse, error) {
	if err := a.requireRunning(req.GetSandboxId()); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	rec := a.writes[req.GetWriteId()]
	if rec == nil {
		if req.GetPath() == "" {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("write not found"))
		}
		if err := a.checkNewID(req.GetWriteId()); err != nil {
			return nil, err
		}
		rec = &writeRec{digest: datapath.WriteDigest(req), path: req.GetPath()}
		a.writes[req.GetWriteId()] = rec
	} else if req.GetPath() != "" && !bytes.Equal(datapath.WriteDigest(req), rec.digest) {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("write digest mismatch"))
	}
	if checkGrant && !bytes.Equal(grantDigest, rec.digest) {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("grant digest does not match the write"))
	}
	if rec.finished != nil {
		if fin := req.GetFinish(); fin != nil && fin.GetSizeBytes() == rec.finished.GetSizeBytes() && fin.GetMode() == rec.finished.GetMode() {
			return &ownsandv1.WriteFileResponse{WrittenBytes: fin.GetSizeBytes(), Finished: true}, nil
		}
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("write already finished"))
	}
	if ch := req.GetChunk(); ch != nil {
		end := ch.GetOffset() + int64(len(ch.GetData()))
		for int64(len(rec.data)) < end {
			rec.data = append(rec.data, 0)
			rec.written = append(rec.written, false)
		}
		for i, b := range ch.GetData() {
			p := ch.GetOffset() + int64(i)
			if rec.written[p] && rec.data[p] != b {
				return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("chunk overlaps written bytes with other content"))
			}
		}
		for i, b := range ch.GetData() {
			p := ch.GetOffset() + int64(i)
			rec.data[p] = b
			rec.written[p] = true
		}
	}
	contiguous := int64(0)
	for contiguous < int64(len(rec.written)) && rec.written[contiguous] {
		contiguous++
	}
	if fin := req.GetFinish(); fin != nil {
		if contiguous != fin.GetSizeBytes() || int64(len(rec.data)) != fin.GetSizeBytes() {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("written ranges do not cover the file"))
		}
		rec.finished = fin
		a.files[rec.path] = bytes.Clone(rec.data)
		return &ownsandv1.WriteFileResponse{WrittenBytes: contiguous, Finished: true}, nil
	}
	return &ownsandv1.WriteFileResponse{WrittenBytes: contiguous}, nil
}
