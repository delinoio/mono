// SPDX-License-Identifier: FSL-1.1-ALv2

package execlog

import (
	"context"
	"io"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Retention rules of the exec contract.
const (
	// KeptOutputBytes is how much of each output stream an exec keeps.
	KeptOutputBytes = 8 << 20
	// Retention is how long an exec and its result stay after it ends.
	Retention = 10 * time.Minute
	// ChunkSize is the largest stdout or stderr chunk of one event.
	ChunkSize = datapath.MaxExecOutputChunkSize
)

// Log holds the started event, the kept stdout and stderr and the final
// event of one exec. It is safe for concurrent use.
type Log struct {
	mu      sync.Mutex
	execID  string
	started *ownsandv1.ExecStarted
	stdout  *Buffer
	stderr  *Buffer
	final   *ownsandv1.ExecEvent
	endTime time.Time
	changed chan struct{}
}

// New returns an empty log that keeps capacity bytes of each stream.
func New(execID string, capacity int) *Log {
	return &Log{
		execID:  execID,
		stdout:  NewBuffer(capacity),
		stderr:  NewBuffer(capacity),
		changed: make(chan struct{}),
	}
}

// ExecID returns the exec ID of the log.
func (l *Log) ExecID() string { return l.execID }

// notifyLocked wakes every cursor that waits for a change.
func (l *Log) notifyLocked() {
	close(l.changed)
	l.changed = make(chan struct{})
}

// Start records the started event. Only the first call has an effect.
func (l *Log) Start(t time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.started != nil {
		return
	}
	l.started = &ownsandv1.ExecStarted{ExecId: l.execID, StartTime: timestamppb.New(t)}
	l.notifyLocked()
}

// WriteStdout appends stdout bytes.
func (l *Log) WriteStdout(p []byte) { l.write(l.stdout, p) }

// WriteStderr appends stderr bytes.
func (l *Log) WriteStderr(p []byte) { l.write(l.stderr, p) }

func (l *Log) write(b *Buffer, p []byte) {
	if len(p) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.final != nil {
		return
	}
	b.Write(p)
	l.notifyLocked()
}

// writeAt appends the part of p at offset off that the buffer has not seen.
// A gap (off beyond the end) resets the kept output to start at off, because
// the bytes in between are no longer available anywhere.
func (l *Log) writeAt(b *Buffer, off int64, p []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.final != nil {
		return
	}
	end := b.End()
	switch {
	case off+int64(len(p)) <= end:
		return
	case off < end:
		p = p[end-off:]
	case off > end:
		*b = Buffer{capacity: b.capacity, start: off, end: off}
	}
	b.Write(p)
	l.notifyLocked()
}

// Finish records the final event (exited, failed or sandbox_ended). Only
// the first call has an effect; it reports whether this call set it.
func (l *Log) Finish(ev *ownsandv1.ExecEvent, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.final != nil {
		return false
	}
	if l.started == nil {
		l.started = &ownsandv1.ExecStarted{ExecId: l.execID, StartTime: timestamppb.New(now)}
	}
	l.final = ev
	l.endTime = now
	l.notifyLocked()
	return true
}

// Apply records one event that another component produced (the agent
// applies the events of the guest helper). Offsets of output events place
// the bytes; repeated bytes are ignored.
func (l *Log) Apply(ev *ownsandv1.ExecEvent, now time.Time) {
	switch e := ev.GetEvent().(type) {
	case *ownsandv1.ExecEvent_Started:
		t := now
		if e.Started.GetStartTime() != nil {
			t = e.Started.GetStartTime().AsTime()
		}
		l.Start(t)
	case *ownsandv1.ExecEvent_Stdout:
		l.writeAt(l.stdout, e.Stdout.GetOffset(), e.Stdout.GetData())
	case *ownsandv1.ExecEvent_Stderr:
		l.writeAt(l.stderr, e.Stderr.GetOffset(), e.Stderr.GetData())
	case *ownsandv1.ExecEvent_Exited, *ownsandv1.ExecEvent_Failed, *ownsandv1.ExecEvent_SandboxEnded:
		l.Finish(ev, now)
	}
}

// Final returns the final event, or nil while the exec runs.
func (l *Log) Final() *ownsandv1.ExecEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.final
}

// EndTime returns when the final event was recorded, or the zero time.
func (l *Log) EndTime() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.endTime
}

// Offsets returns the next offsets of stdout and stderr: the total bytes
// that each stream produced.
func (l *Log) Offsets() (stdout, stderr int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stdout.End(), l.stderr.End()
}

// Cursor returns a reader of the event sequence that starts with the
// started event and continues from the given offsets.
func (l *Log) Cursor(stdoutOffset, stderrOffset int64) *Cursor {
	return &Cursor{log: l, stdoutOff: stdoutOffset, stderrOff: stderrOffset}
}

type cursorPhase int

const (
	phaseStarted cursorPhase = iota
	phaseTruncated
	phaseOutput
	phaseDone
)

// Cursor reads the event sequence of a log: started, an optional
// output_truncated when the requested offsets are older than the kept
// output, stdout and stderr chunks of at most ChunkSize bytes, and the final
// event. A cursor is not safe for concurrent use.
type Cursor struct {
	log       *Log
	stdoutOff int64
	stderrOff int64
	phase     cursorPhase
	preferErr bool
}

// Done reports whether the cursor returned the final event.
func (c *Cursor) Done() bool { return c.phase == phaseDone }

// Offsets returns the next stdout and stderr offsets of the cursor.
func (c *Cursor) Offsets() (stdout, stderr int64) { return c.stdoutOff, c.stderrOff }

// TryNext returns the next event when one is available. Otherwise it
// returns nil and a channel that is closed when the log changes. After the
// final event it returns nil and a nil channel.
func (c *Cursor) TryNext() (*ownsandv1.ExecEvent, <-chan struct{}) {
	l := c.log
	l.mu.Lock()
	defer l.mu.Unlock()
	for {
		switch c.phase {
		case phaseDone:
			return nil, nil
		case phaseStarted:
			if l.started == nil {
				return nil, l.changed
			}
			c.phase = phaseTruncated
			return &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Started{Started: l.started}}, nil
		case phaseTruncated:
			c.phase = phaseOutput
			if c.stdoutOff < l.stdout.Start() || c.stderrOff < l.stderr.Start() {
				c.stdoutOff = max(c.stdoutOff, l.stdout.Start())
				c.stderrOff = max(c.stderrOff, l.stderr.Start())
				return &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_OutputTruncated{
					OutputTruncated: &ownsandv1.ExecOutputTruncated{StdoutOffset: c.stdoutOff, StderrOffset: c.stderrOff},
				}}, nil
			}
		case phaseOutput:
			// A consumer that fell behind the kept output skips to the
			// oldest kept byte; the chunk offsets show the gap.
			c.stdoutOff = max(c.stdoutOff, l.stdout.Start())
			c.stderrOff = max(c.stderrOff, l.stderr.Start())
			order := [2]bool{c.preferErr, !c.preferErr}
			for _, isErr := range order {
				buf, off := l.stdout, &c.stdoutOff
				if isErr {
					buf, off = l.stderr, &c.stderrOff
				}
				data := buf.Read(*off, ChunkSize)
				if len(data) == 0 {
					continue
				}
				out := &ownsandv1.ExecOutput{Offset: *off, Data: data}
				*off += int64(len(data))
				c.preferErr = !isErr
				if isErr {
					return &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Stderr{Stderr: out}}, nil
				}
				return &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Stdout{Stdout: out}}, nil
			}
			if l.final != nil {
				c.phase = phaseDone
				return l.final, nil
			}
			return nil, l.changed
		}
	}
}

// Next waits for the next event. When keepalive is positive and no event
// arrives within it, Next returns a keepalive event. It returns io.EOF after
// the final event and the context error when ctx ends.
func (c *Cursor) Next(ctx context.Context, keepalive time.Duration) (*ownsandv1.ExecEvent, error) {
	var timeout <-chan time.Time
	if keepalive > 0 {
		timer := time.NewTimer(keepalive)
		defer timer.Stop()
		timeout = timer.C
	}
	for {
		ev, changed := c.TryNext()
		if ev != nil {
			return ev, nil
		}
		if changed == nil {
			return nil, io.EOF
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timeout:
			return KeepaliveEvent(), nil
		case <-changed:
		}
	}
}

// KeepaliveEvent returns an exec keepalive event.
func KeepaliveEvent() *ownsandv1.ExecEvent {
	return &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Keepalive{Keepalive: &ownsandv1.Keepalive{}}}
}

// IsFinal reports whether ev ends the event sequence.
func IsFinal(ev *ownsandv1.ExecEvent) bool {
	switch ev.GetEvent().(type) {
	case *ownsandv1.ExecEvent_Exited, *ownsandv1.ExecEvent_Failed, *ownsandv1.ExecEvent_SandboxEnded:
		return true
	}
	return false
}
