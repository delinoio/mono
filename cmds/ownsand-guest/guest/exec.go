package guest

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// ring keeps the last limit bytes of an output stream and the total number of
// bytes written, so offsets stay absolute.
type ring struct {
	buf   []byte
	limit int
	end   int64
}

func (r *ring) write(p []byte) {
	for len(p) > 0 {
		if len(r.buf) < r.limit {
			n := min(len(p), r.limit-len(r.buf))
			r.buf = append(r.buf, p[:n]...)
			r.end += int64(n)
			p = p[n:]
			continue
		}
		pos := int(r.end % int64(r.limit))
		n := min(len(p), r.limit-pos)
		copy(r.buf[pos:], p[:n])
		r.end += int64(n)
		p = p[n:]
	}
}

// first returns the offset of the oldest kept byte.
func (r *ring) first() int64 { return r.end - int64(len(r.buf)) }

// read returns up to n kept bytes starting at off (off must be kept).
func (r *ring) read(off int64, n int) []byte {
	if off < r.first() || off >= r.end {
		return nil
	}
	n = int(min(int64(n), r.end-off))
	out := make([]byte, 0, n)
	for len(out) < n {
		pos := int(off % int64(r.limit))
		m := min(n-len(out), len(r.buf)-pos)
		out = append(out, r.buf[pos:pos+m]...)
		off += int64(m)
	}
	return out
}

// execRecord is one exec and its kept output and result.
type execRecord struct {
	id     string
	digest []byte

	mu       sync.Mutex
	changed  chan struct{}
	stdout   ring
	stderr   ring
	done     bool
	result   *ownsandv1.ExecEvent
	ended    time.Time
	timedOut bool
}

func newExecRecord(id string, digest []byte, retention int) *execRecord {
	return &execRecord{
		id:      id,
		digest:  digest,
		changed: make(chan struct{}),
		stdout:  ring{limit: retention},
		stderr:  ring{limit: retention},
	}
}

func (e *execRecord) notifyLocked() {
	close(e.changed)
	e.changed = make(chan struct{})
}

func (e *execRecord) endedAt() (bool, time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.done, e.ended
}

func (e *execRecord) appendOutput(stream ownsandv1.OutputStream, p []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.done {
		return
	}
	if stream == ownsandv1.OutputStream_OUTPUT_STREAM_STDOUT {
		e.stdout.write(p)
	} else {
		e.stderr.write(p)
	}
	e.notifyLocked()
}

func (e *execRecord) finish(result *ownsandv1.ExecEvent, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.done {
		return
	}
	e.done = true
	e.result = result
	e.ended = now
	e.notifyLocked()
}

// serveExec starts a new exec or attaches to a known one with the same
// request binding.
func (s *Server) serveExec(c *conn, st *stream, req *ownsandv1.ExecRequest) error {
	if !datapath.IsUUIDv7(req.GetExecId()) {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("exec_id must be a UUID v7"))
	}
	if len(req.GetCommand()) == 0 || req.GetCommand()[0] == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("command must name a program"))
	}
	if err := checkExecSize(req); err != nil {
		return err
	}
	digest := datapath.ExecDigest(req)
	s.mu.Lock()
	if s.tombstonedLocked(req.GetExecId()) {
		s.mu.Unlock()
		return errNotFound("exec")
	}
	if e, ok := s.execs[req.GetExecId()]; ok {
		s.mu.Unlock()
		if !bytes.Equal(e.digest, digest) {
			return connect.NewError(connect.CodeAlreadyExists, errors.New("exec_id is already used by another request"))
		}
		return s.subscribe(c, st, e, 0, 0)
	}
	e := newExecRecord(req.GetExecId(), digest, s.cfg.OutputRetention)
	s.execs[req.GetExecId()] = e
	s.mu.Unlock()

	s.log.Info("exec starting", "exec_id", e.id, "stream_id", st.id)
	s.startProcess(e, req)
	return s.subscribe(c, st, e, 0, 0)
}

func checkExecSize(req *ownsandv1.ExecRequest) error {
	n := len(req.GetWorkingDirectory())
	for _, a := range req.GetCommand() {
		n += len(a) + 2
	}
	for k, v := range req.GetEnv() {
		n += len(k) + len(v) + 4
	}
	if n > datapath.MaxExecRequestFields {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("command, env and working directory exceed 256 KiB"))
	}
	return nil
}

// startProcess runs the command and records its output and result. A start
// failure becomes a failed event.
func (s *Server) startProcess(e *execRecord, req *ownsandv1.ExecRequest) {
	cmd := exec.Command(req.GetCommand()[0], req.GetCommand()[1:]...)
	cmd.Dir = req.GetWorkingDirectory()
	// The command inherits the guest helper environment, and the request env
	// overrides it.
	cmd.Env = os.Environ()
	for k, v := range req.GetEnv() {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	prepareProcess(cmd)

	outR, outW, err := os.Pipe()
	if err != nil {
		s.failExec(e, "create stdout pipe failed")
		return
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = outR.Close()
		_ = outW.Close()
		s.failExec(e, "create stderr pipe failed")
		return
	}
	cmd.Stdout = outW
	cmd.Stderr = errW
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		_ = outR.Close()
		_ = outW.Close()
		_ = errR.Close()
		_ = errW.Close()
		// The error text names the program, which is sandbox content: it goes
		// to the caller only, never to the log.
		s.failExec(e, err.Error())
		s.log.Info("exec failed to start", "exec_id", e.id)
		return
	}
	_ = outW.Close()
	_ = errW.Close()

	var readers sync.WaitGroup
	readers.Add(2)
	pump := func(r *os.File, stream ownsandv1.OutputStream) {
		defer readers.Done()
		defer r.Close()
		buf := make([]byte, 32<<10)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				e.appendOutput(stream, buf[:n])
			}
			if err != nil {
				return
			}
		}
	}
	go pump(outR, ownsandv1.OutputStream_OUTPUT_STREAM_STDOUT)
	go pump(errR, ownsandv1.OutputStream_OUTPUT_STREAM_STDERR)

	var timer *time.Timer
	var timedOut bool
	var tmu sync.Mutex
	if d := req.GetTimeout(); d != nil && d.AsDuration() > 0 {
		timer = time.AfterFunc(d.AsDuration(), func() {
			tmu.Lock()
			timedOut = true
			tmu.Unlock()
			s.log.Info("exec timed out, terminating", "exec_id", e.id)
			terminateProcess(cmd, s.cfg.KillGrace)
		})
	}

	go func() {
		waitErr := cmd.Wait()
		if timer != nil {
			timer.Stop()
		}
		// Wait for the output pipes to drain. A background process that keeps
		// the pipes open must not keep the exec running forever, so the wait
		// is bounded.
		drained := make(chan struct{})
		go func() {
			readers.Wait()
			close(drained)
		}()
		select {
		case <-drained:
		case <-time.After(2 * time.Second):
		}
		tmu.Lock()
		to := timedOut
		tmu.Unlock()
		ev := exitEvent(cmd, waitErr, to)
		e.finish(ev, s.cfg.Now())
		s.log.Info("exec ended", "exec_id", e.id, "timed_out", to)
	}()
}

func (s *Server) failExec(e *execRecord, reason string) {
	e.finish(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Failed_{Failed: &ownsandv1.ExecEvent_Failed{Reason: reason}}}, s.cfg.Now())
}

func exitEvent(cmd *exec.Cmd, waitErr error, timedOut bool) *ownsandv1.ExecEvent {
	ex := &ownsandv1.ExecEvent_Exited{TimedOut: timedOut}
	if sig, ok := exitSignal(cmd.ProcessState); ok {
		ex.Result = &ownsandv1.ExecEvent_Exited_Signal{Signal: int32(sig)}
	} else if cmd.ProcessState != nil {
		ex.Result = &ownsandv1.ExecEvent_Exited_ExitCode{ExitCode: int64(cmd.ProcessState.ExitCode())}
	} else {
		var ee *exec.ExitError
		code := int64(-1)
		if errors.As(waitErr, &ee) {
			code = int64(ee.ExitCode())
		}
		ex.Result = &ownsandv1.ExecEvent_Exited_ExitCode{ExitCode: code}
	}
	return &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Exited_{Exited: ex}}
}

// serveAttach continues the event stream of a known exec.
func (s *Server) serveAttach(c *conn, st *stream, req *ownsandv1.AttachExecRequest) error {
	if req.GetStdoutOffset() < 0 || req.GetStderrOffset() < 0 {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("offsets must not be negative"))
	}
	s.mu.Lock()
	e, ok := s.execs[req.GetExecId()]
	s.mu.Unlock()
	if !ok {
		return errNotFound("exec")
	}
	return s.subscribe(c, st, e, req.GetStdoutOffset(), req.GetStderrOffset())
}

// subscribe streams the exec events from the given offsets: started, an
// output_truncated event when an offset is older than the kept output, the
// output chunks and the result. When a slow subscriber falls behind the kept
// output while the exec runs, the stream carries another output_truncated
// event at that point with the new first offsets.
func (s *Server) subscribe(c *conn, st *stream, e *execRecord, outOff, errOff int64) error {
	send := func(ev *ownsandv1.ExecEvent) error {
		return c.sendPayload(st, func(f *ownsandv1.DataFrame) {
			f.Payload = &ownsandv1.DataFrame_ExecEvent{ExecEvent: ev}
		})
	}
	if err := send(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Started_{Started: &ownsandv1.ExecEvent_Started{ExecId: e.id}}}); err != nil {
		return err
	}
	keepalive := time.NewTimer(ExecKeepaliveInterval)
	defer keepalive.Stop()
	for {
		e.mu.Lock()
		fo, fe := e.stdout.first(), e.stderr.first()
		var truncated *ownsandv1.ExecEvent
		if outOff < fo || errOff < fe {
			outOff, errOff = max(outOff, fo), max(errOff, fe)
			truncated = &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_OutputTruncated_{OutputTruncated: &ownsandv1.ExecEvent_OutputTruncated{
				FirstStdoutOffset: fo, FirstStderrOffset: fe,
			}}}
		}
		var out *ownsandv1.ExecEvent
		if outOff > e.stdout.end {
			outOff = e.stdout.end
		}
		if errOff > e.stderr.end {
			errOff = e.stderr.end
		}
		if data := e.stdout.read(outOff, datapath.MaxExecOutputChunk); len(data) > 0 {
			out = outputEvent(ownsandv1.OutputStream_OUTPUT_STREAM_STDOUT, outOff, data)
			outOff += int64(len(data))
		} else if data := e.stderr.read(errOff, datapath.MaxExecOutputChunk); len(data) > 0 {
			out = outputEvent(ownsandv1.OutputStream_OUTPUT_STREAM_STDERR, errOff, data)
			errOff += int64(len(data))
		}
		done, result, ch := e.done, e.result, e.changed
		e.mu.Unlock()

		if truncated != nil {
			if err := send(truncated); err != nil {
				return err
			}
		}
		if out != nil {
			if err := send(out); err != nil {
				return err
			}
			keepalive.Reset(ExecKeepaliveInterval)
			continue
		}
		if done {
			return send(result)
		}
		select {
		case <-ch:
		case <-keepalive.C:
			if err := send(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Keepalive_{Keepalive: &ownsandv1.ExecEvent_Keepalive{}}}); err != nil {
				return err
			}
			keepalive.Reset(ExecKeepaliveInterval)
		case <-st.ctx.Done():
			return st.ctx.Err()
		}
	}
}

func outputEvent(stream ownsandv1.OutputStream, off int64, data []byte) *ownsandv1.ExecEvent {
	return &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Output_{Output: &ownsandv1.ExecEvent_Output{Stream: stream, Offset: off, Data: data}}}
}
