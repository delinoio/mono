package datapath

import (
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/guestconn"
	sdkdp "github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// execMirror is the agent's copy of one exec: it reads the exec's events
// from the guest helper into its own output buffers, so the agent keeps the
// last 8 MiB of each output stream and the result, also while the sandbox
// is paused and the guest cannot answer. Every transport stream of the exec
// reads from the mirror.
type execMirror struct {
	id        string
	sandboxID string
	digest    []byte

	mu      sync.Mutex
	changed chan struct{}
	stdout  ring
	stderr  ring
	done    bool
	result  *ownsandv1.ExecEvent
	errCode connect.Code
	errMsg  string
	ended   time.Time
}

func newExecMirror(id, sandboxID string, digest []byte) *execMirror {
	return &execMirror{
		id: id, sandboxID: sandboxID, digest: digest,
		changed: make(chan struct{}),
		stdout:  ring{limit: OutputRetention},
		stderr:  ring{limit: OutputRetention},
	}
}

func (m *execMirror) notifyLocked() {
	close(m.changed)
	m.changed = make(chan struct{})
}

func (m *execMirror) endedAt() (bool, time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.done, m.ended
}

func (m *execMirror) finish(result *ownsandv1.ExecEvent, code connect.Code, msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.done {
		return
	}
	m.done, m.result, m.errCode, m.errMsg, m.ended = true, result, code, msg, time.Now()
	m.notifyLocked()
}

// add records an output chunk at its offset. A chunk after a gap moves the
// buffer forward; a repeated chunk (after a reconnection) is trimmed.
func (m *execMirror) add(o *ownsandv1.ExecEvent_Output) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := &m.stdout
	if o.GetStream() == ownsandv1.OutputStream_OUTPUT_STREAM_STDERR {
		r = &m.stderr
	}
	data, off := o.GetData(), o.GetOffset()
	r.skipTo(off)
	if off < r.end {
		skip := r.end - off
		if skip >= int64(len(data)) {
			return
		}
		data = data[skip:]
	}
	r.write(data)
	m.notifyLocked()
}

func (m *execMirror) truncated(t *ownsandv1.ExecEvent_OutputTruncated) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stdout.skipTo(t.GetFirstStdoutOffset())
	m.stderr.skipTo(t.GetFirstStderrOffset())
	m.notifyLocked()
}

func isTerminal(st ownsandv1.SandboxState) bool {
	return st == ownsandv1.SandboxState_SANDBOX_STATE_DELETED || st == ownsandv1.SandboxState_SANDBOX_STATE_FAILED ||
		st == ownsandv1.SandboxState_SANDBOX_STATE_DELETING || st == ownsandv1.SandboxState_SANDBOX_STATE_UNSPECIFIED
}

func (d *Dispatcher) sandboxEnded(sid string) *ownsandv1.ExecEvent {
	reason := d.sb.Report(sid).GetEndReason()
	return &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_SandboxEnded_{SandboxEnded: &ownsandv1.ExecEvent_SandboxEnded{Reason: reason}}}
}

// runMirror starts the exec in the guest and follows it until it ends. When
// the guest connection drops, it attaches again from the next offsets.
func (d *Dispatcher) runMirror(m *execMirror, req *ownsandv1.ExecRequest) {
	sid := m.sandboxID
	end := d.sb.BeginActivity(sid)
	defer end()
	started := false
	backoff := 100 * time.Millisecond
	for {
		if d.ctx.Err() != nil {
			m.finish(d.sandboxEnded(sid), 0, "")
			return
		}
		st := d.sb.SandboxState(sid)
		if isTerminal(st) {
			m.finish(d.sandboxEnded(sid), 0, "")
			d.log.Info("exec ended with its sandbox", "sandbox_id", sid, "exec_id", m.id)
			return
		}
		gc, err := d.sb.Guest(d.ctx, sid)
		var frame *ownsandv1.DataFrame
		if err == nil {
			if !started {
				frame = sdkdp.NewFrame(0, sid, func(f *ownsandv1.DataFrame) { f.Payload = &ownsandv1.DataFrame_ExecRequest{ExecRequest: req} })
			} else {
				m.mu.Lock()
				outOff, errOff := m.stdout.end, m.stderr.end
				m.mu.Unlock()
				frame = sdkdp.NewFrame(0, sid, func(f *ownsandv1.DataFrame) {
					f.Payload = &ownsandv1.DataFrame_ExecAttachRequest{ExecAttachRequest: &ownsandv1.AttachExecRequest{
						SandboxId: sid, ExecId: m.id, StdoutOffset: outOff, StderrOffset: errOff,
					}}
				})
			}
			if gs, oerr := gc.Open(frame); oerr == nil {
				done := d.followGuest(m, gs, &started)
				gs.Close()
				if done {
					return
				}
				backoff = 100 * time.Millisecond
			}
		}
		select {
		case <-d.ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, 5*time.Second)
	}
}

// followGuest reads exec events from the guest into the mirror. It returns
// true when the exec ended.
func (d *Dispatcher) followGuest(m *execMirror, gs *guestconn.Stream, started *bool) bool {
	for {
		f, err := gs.Recv(d.ctx)
		if err != nil {
			return false
		}
		if e := f.GetError(); e != nil {
			code := codeFromName(e.GetCode())
			if *started && code == connect.CodeNotFound {
				// The guest no longer knows the exec (for example, after a
				// guest restart): its outcome is unknown.
				m.finish(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Failed_{Failed: &ownsandv1.ExecEvent_Failed{Reason: "the guest helper lost the exec"}}}, 0, "")
				return true
			}
			m.finish(nil, code, e.GetMessage())
			return true
		}
		ev := f.GetExecEvent()
		switch {
		case ev.GetStarted() != nil:
			*started = true
		case ev.GetOutputTruncated() != nil:
			m.truncated(ev.GetOutputTruncated())
		case ev.GetOutput() != nil:
			m.add(ev.GetOutput())
		case ev.GetExited() != nil || ev.GetFailed() != nil:
			m.finish(ev, 0, "")
			d.log.Info("exec ended", "sandbox_id", m.sandboxID, "exec_id", m.id)
			return true
		}
	}
}

// subscribe sends the exec's events from the given offsets to sink:
// started, output_truncated when an offset is older than the kept output,
// the output, and the result. On a paused sandbox, an exec that has not
// ended returns its kept output and then a paused event.
func (d *Dispatcher) subscribe(sid string, m *execMirror, outOff, errOff int64, sink Sink) Outcome {
	ctx := sink.Context()
	send := func(ev *ownsandv1.ExecEvent) error {
		return sink.Payload(func(f *ownsandv1.DataFrame) { f.Payload = &ownsandv1.DataFrame_ExecEvent{ExecEvent: ev} })
	}
	if err := send(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Started_{Started: &ownsandv1.ExecEvent_Started{ExecId: m.id}}}); err != nil {
		return Outcome{Started: true}
	}
	keepalive := time.NewTimer(keepaliveInterval)
	defer keepalive.Stop()
	pollPaused := time.NewTicker(250 * time.Millisecond)
	defer pollPaused.Stop()
	for {
		m.mu.Lock()
		fo, fe := m.stdout.first(), m.stderr.first()
		var truncated, out *ownsandv1.ExecEvent
		if outOff < fo || errOff < fe {
			outOff, errOff = max(outOff, fo), max(errOff, fe)
			truncated = &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_OutputTruncated_{OutputTruncated: &ownsandv1.ExecEvent_OutputTruncated{FirstStdoutOffset: fo, FirstStderrOffset: fe}}}
		}
		outOff, errOff = min(outOff, m.stdout.end), min(errOff, m.stderr.end)
		if data := m.stdout.read(outOff, sdkdp.MaxExecOutputChunk); len(data) > 0 {
			out = &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Output_{Output: &ownsandv1.ExecEvent_Output{Stream: ownsandv1.OutputStream_OUTPUT_STREAM_STDOUT, Offset: outOff, Data: data}}}
			outOff += int64(len(data))
		} else if data := m.stderr.read(errOff, sdkdp.MaxExecOutputChunk); len(data) > 0 {
			out = &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Output_{Output: &ownsandv1.ExecEvent_Output{Stream: ownsandv1.OutputStream_OUTPUT_STREAM_STDERR, Offset: errOff, Data: data}}}
			errOff += int64(len(data))
		}
		done, result, code, msg, ch := m.done, m.result, m.errCode, m.errMsg, m.changed
		m.mu.Unlock()

		if truncated != nil {
			if err := send(truncated); err != nil {
				return Outcome{Started: true}
			}
		}
		if out != nil {
			if err := send(out); err != nil {
				return Outcome{Started: true}
			}
			keepalive.Reset(keepaliveInterval)
			continue
		}
		if done {
			if result == nil {
				_ = sink.Error(code, msg)
				return Outcome{Code: code}
			}
			if err := send(result); err != nil {
				return Outcome{Started: true}
			}
			return Outcome{Finished: true, Started: true}
		}
		if d.sb.SandboxState(sid) == ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
			if err := send(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Paused_{Paused: &ownsandv1.ExecEvent_Paused{}}}); err != nil {
				return Outcome{Started: true}
			}
			return Outcome{Started: true}
		}
		select {
		case <-ch:
		case <-pollPaused.C:
		case <-keepalive.C:
			if err := send(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Keepalive_{Keepalive: &ownsandv1.ExecEvent_Keepalive{}}}); err != nil {
				return Outcome{Started: true}
			}
			keepalive.Reset(keepaliveInterval)
		case <-ctx.Done():
			return Outcome{Started: true}
		}
	}
}
