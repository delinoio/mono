// SPDX-License-Identifier: Apache-2.0

// Package eventlog writes bridge events: each event except
// credential_update is appended to the guest event log `events.jsonl` as
// one line of protobuf JSON and then written to stdout as the same line.
// The owning contract is docs/cmds-sandcrew-bridge-foundation.md#event-log.
package eventlog

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	sandcrewv1 "github.com/delinoio/mono/protos/delino/sandcrew/v1"
)

// ErrWriteFailed is returned after an append failed and its retries
// failed. The log accepts no further events.
var ErrWriteFailed = errors.New("eventlog: event log write failed")

// appendRetries is the number of retries of a failed append.
const appendRetries = 3

// SyncInterval is the interval of fsync while lines were written.
const SyncInterval = 5 * time.Second

// File is the append-only log file.
type File interface {
	io.Writer
	Sync() error
	Close() error
}

// Options configure a Log.
type Options struct {
	// Stdout receives every line. Write errors are logged and ignored:
	// the control plane recovers missed events from the log file.
	Stdout io.Writer
	Logger *slog.Logger
	// Now returns the event time. Defaults to time.Now.
	Now func() time.Time
	// OpenFile opens the log for appending. Defaults to os.OpenFile with
	// O_APPEND.
	OpenFile func(path string) (File, error)
	// RetryDelay is the delay between append retries. Defaults to 100ms.
	RetryDelay time.Duration
}

// Log is the event writer of one bridge process.
type Log struct {
	opts Options

	mu       sync.Mutex
	file     File
	sequence uint64
	offset   uint64
	inputs   map[string]struct{}
	dirty    bool
	failed   bool
}

// Open opens or creates the log at path. It reads the existing lines to
// continue the sequence and to find the input IDs that the log already
// holds, so a restarted bridge never rewrites the log or repeats an input.
func Open(path string, opts Options) (*Log, error) {
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.OpenFile == nil {
		opts.OpenFile = func(path string) (File, error) {
			return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		}
	}
	if opts.RetryDelay == 0 {
		opts.RetryDelay = 100 * time.Millisecond
	}
	l := &Log{opts: opts, inputs: make(map[string]struct{})}
	terminated, err := l.recover(path)
	if err != nil {
		return nil, err
	}
	file, err := opts.OpenFile(path)
	if err != nil {
		return nil, fmt.Errorf("eventlog: open: %w", err)
	}
	l.file = file
	if !terminated {
		// A crash left a last line without a newline: a partial event, or
		// a complete one whose newline is missing. Terminate it, so the next
		// event starts on its own line; the line stays as it is.
		opts.Logger.Warn("event log ends with a partial line", "log_end_offset", l.offset)
		if _, err := file.Write([]byte("\n")); err != nil {
			file.Close()
			return nil, fmt.Errorf("eventlog: terminate partial line: %w", err)
		}
		l.offset++
	}
	return l, nil
}

// recover reads the existing log. It reports whether the log is empty or
// ends with a newline.
func (l *Log) recover(path string) (bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("eventlog: read: %w", err)
	}
	defer f.Close()
	reader := bufio.NewReaderSize(f, 64<<10)
	unmarshal := protojson.UnmarshalOptions{DiscardUnknown: true}
	for {
		line, err := reader.ReadBytes('\n')
		l.offset += uint64(len(line))
		// The last line can lack its newline. When it still holds a
		// complete event, that event counts: Open adds the newline, so the
		// event stays in the log, and the next event must not reuse its
		// sequence or apply its input again.
		if len(bytes.TrimSpace(line)) > 0 {
			var event sandcrewv1.BridgeEvent
			if uerr := unmarshal.Unmarshal(bytes.TrimSpace(line), &event); uerr != nil {
				l.opts.Logger.Warn("event log line does not parse", "log_end_offset", l.offset, "error", uerr)
			} else {
				l.sequence = max(l.sequence, event.GetSequence())
				l.rememberInput(&event)
			}
		}
		if errors.Is(err, io.EOF) {
			return len(line) == 0, nil
		}
		if err != nil {
			return false, fmt.Errorf("eventlog: read: %w", err)
		}
	}
}

func (l *Log) rememberInput(event *sandcrewv1.BridgeEvent) {
	switch e := event.GetEvent().(type) {
	case *sandcrewv1.BridgeEvent_InputAccepted:
		l.inputs[e.InputAccepted.GetInputId()] = struct{}{}
	case *sandcrewv1.BridgeEvent_InputRejected:
		l.inputs[e.InputRejected.GetInputId()] = struct{}{}
	}
}

// HasInput reports whether the log holds an input_accepted or
// input_rejected event of id.
func (l *Log) HasInput(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.inputs[id]
	return ok
}

// LastSequence returns the sequence of the last logged event.
func (l *Log) LastSequence() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sequence
}

// Append sets the sequence, the log end offset and the time of event,
// appends it to the log and writes it to stdout. A turn_ended event is
// synced to disk. After a failed append and its retries, Append writes a
// bridge_error EVENT_LOG_WRITE_FAILED to stdout and returns ErrWriteFailed.
func (l *Log) Append(event *sandcrewv1.BridgeEvent) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failed {
		return ErrWriteFailed
	}
	event.Sequence = l.sequence + 1
	event.Time = timestamppb.New(l.opts.Now())
	// The offset depends on the line length, which depends on the offset.
	// Iterate until the encoded length is stable (at most a few rounds,
	// because the digits of the offset change rarely).
	var line []byte
	offset := l.offset
	for range 4 {
		event.LogEndOffset = offset
		var err error
		if line, err = encode(event); err != nil {
			return fmt.Errorf("eventlog: encode: %w", err)
		}
		next := l.offset + uint64(len(line))
		if next == offset {
			break
		}
		offset = next
	}
	if event.LogEndOffset != l.offset+uint64(len(line)) {
		return errors.New("eventlog: log end offset does not converge")
	}
	if err := l.writeWithRetries(line); err != nil {
		l.failed = true
		l.opts.Logger.Error("event log write failed", "sequence", event.Sequence, "error", err)
		l.writeStdout(l.failureEvent())
		return ErrWriteFailed
	}
	l.sequence = event.Sequence
	l.offset = event.LogEndOffset
	l.dirty = true
	l.rememberInput(event)
	if _, ok := event.GetEvent().(*sandcrewv1.BridgeEvent_TurnEnded); ok {
		l.syncLocked()
	}
	l.writeStdout(line)
	return nil
}

// WriteStdoutOnly writes event to stdout only, with the sequence and the
// log end offset of the last logged event. It is used for
// credential_update, which never enters the log.
func (l *Log) WriteStdoutOnly(event *sandcrewv1.BridgeEvent) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	event.Sequence = l.sequence
	event.LogEndOffset = l.offset
	event.Time = timestamppb.New(l.opts.Now())
	line, err := encode(event)
	if err != nil {
		return fmt.Errorf("eventlog: encode: %w", err)
	}
	l.writeStdout(line)
	return nil
}

// Sync flushes the log file to disk when lines were written since the last
// sync.
func (l *Log) Sync() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.syncLocked()
}

// RunSyncer syncs the log every SyncInterval until ctx ends.
func (l *Log) RunSyncer(ctx context.Context) {
	ticker := time.NewTicker(SyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.Sync()
		}
	}
}

// Close syncs and closes the log file.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.syncLocked()
	return l.file.Close()
}

func (l *Log) syncLocked() {
	if !l.dirty || l.failed {
		return
	}
	if err := l.file.Sync(); err != nil {
		l.opts.Logger.Warn("event log sync failed", "error", err)
		return
	}
	l.dirty = false
}

func (l *Log) writeWithRetries(line []byte) error {
	var err error
	rest := line
	for attempt := 0; attempt <= appendRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(l.opts.RetryDelay)
		}
		var n int
		n, err = l.file.Write(rest)
		// A partial write is continued, so the line is never repeated.
		rest = rest[n:]
		if err == nil && len(rest) == 0 {
			return nil
		}
		if err == nil {
			err = io.ErrShortWrite
		}
		l.opts.Logger.Warn("event log append failed", "attempt", attempt+1, "error", err)
	}
	return err
}

func (l *Log) failureEvent() []byte {
	event := &sandcrewv1.BridgeEvent{
		Sequence:     l.sequence,
		LogEndOffset: l.offset,
		Time:         timestamppb.New(l.opts.Now()),
		Event: &sandcrewv1.BridgeEvent_BridgeError{BridgeError: &sandcrewv1.BridgeError{
			Kind:    sandcrewv1.BridgeErrorKind_BRIDGE_ERROR_KIND_EVENT_LOG_WRITE_FAILED,
			Message: fmt.Sprintf("the event log could not be appended after sequence %d", l.sequence),
		}},
	}
	line, err := encode(event)
	if err != nil {
		return nil
	}
	return line
}

func (l *Log) writeStdout(line []byte) {
	if len(line) == 0 {
		return
	}
	if _, err := l.opts.Stdout.Write(line); err != nil {
		l.opts.Logger.Warn("stdout write failed", "error", err)
	}
}

// encode returns event as one compact line of protobuf JSON with a newline.
// protojson adds random whitespace on purpose; Compact removes it, so the
// line is stable.
func encode(event *sandcrewv1.BridgeEvent) ([]byte, error) {
	data, err := protojson.Marshal(event)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, data); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// Decode parses one event line.
func Decode(line []byte) (*sandcrewv1.BridgeEvent, error) {
	var event sandcrewv1.BridgeEvent
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(bytes.TrimSpace(line), &event); err != nil {
		return nil, err
	}
	return &event, nil
}
