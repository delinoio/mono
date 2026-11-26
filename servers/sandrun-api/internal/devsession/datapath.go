// SPDX-License-Identifier: FSL-1.1-ALv2

package devsession

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/go/ownsand"
	"github.com/delinoio/mono/go/uuidv7"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient"
)

// Binding salts (recorded decision): SandRun derives the OwnSand binding salt
// of a relayed read or write from its operation ID with the primary key of
// SANDRUN_SECRET_KEYS, so every call of one read_id or write_id uses the same
// salt, also on another instance after a deployment handover. A rotation of
// the primary key during an operation changes the salt; the client then
// starts the transfer again with a new operation ID.
func (h *Handler) bindingSalt(sandboxID, opID string) []byte {
	return h.svc.Secrets.Derive("ownsand-binding-salt", sandboxID+"/"+opID)
}

// lowerUUIDv7 validates a client-generated operation ID and returns its
// lowercase form, which OwnSand requires for exec IDs.
func lowerUUIDv7(field, value string) (string, error) {
	id, err := uuidv7.Parse(value)
	if err != nil {
		return "", api.ErrInvalidf("invalid %s: must be a UUID v7", field)
	}
	return id.String(), nil
}

// execSender serializes the sends of one exec stream and records when the
// last event went out.
type execSender struct {
	mu   sync.Mutex
	send func(*sandrunv1.DevSessionExecEvent) error
	last time.Time
}

func (s *execSender) Send(ev *sandrunv1.DevSessionExecEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = time.Now()
	return s.send(ev)
}

func (s *execSender) idle() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.last)
}

// execOffsets tracks the next byte of each output stream in one relay.
type execOffsets struct {
	stdout int64
	stderr int64
}

// convertEvent copies an OwnSand exec event into the SandRun schema and advances the offsets.
func (o *execOffsets) convertEvent(ev ownsand.ExecEvent) *sandrunv1.DevSessionExecEvent {
	switch ev.Kind {
	case ownsand.ExecEventStarted:
		started := &sandrunv1.DevSessionExecStarted{ExecId: ev.ExecID}
		if ev.StartTime != nil {
			started.StartTime = timestamppb.New(*ev.StartTime)
		}
		return &sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_Started{Started: started}}
	case ownsand.ExecEventStdout:
		o.stdout = max(o.stdout, ev.Offset+int64(len(ev.Data)))
		return &sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_Stdout{Stdout: &sandrunv1.DevSessionExecOutput{Offset: ev.Offset, Data: ev.Data}}}
	case ownsand.ExecEventStderr:
		o.stderr = max(o.stderr, ev.Offset+int64(len(ev.Data)))
		return &sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_Stderr{Stderr: &sandrunv1.DevSessionExecOutput{Offset: ev.Offset, Data: ev.Data}}}
	case ownsand.ExecEventOutputTruncated:
		if !ev.MissingStdout.Empty() {
			o.stdout = max(o.stdout, ev.MissingStdout.To)
		}
		if !ev.MissingStderr.Empty() {
			o.stderr = max(o.stderr, ev.MissingStderr.To)
		}
		return &sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_OutputTruncated{OutputTruncated: &sandrunv1.DevSessionExecOutputTruncated{
			StdoutOffset: o.stdout, StderrOffset: o.stderr}}}
	}
	return nil
}

// resultEvent returns the final event of an exec result and its metric
// result label.
func resultEvent(res *ownsand.ExecResult) (*sandrunv1.DevSessionExecEvent, string) {
	switch res.Kind {
	case ownsand.ExecResultExited:
		ex := &sandrunv1.DevSessionExecExited{TimedOut: res.TimedOut}
		if res.EndTime != nil {
			ex.EndTime = timestamppb.New(*res.EndTime)
		}
		switch {
		case res.ExitCode != nil:
			ex.Status = &sandrunv1.DevSessionExecExited_ExitCode{ExitCode: *res.ExitCode}
		case res.Signal != nil:
			ex.Status = &sandrunv1.DevSessionExecExited_Signal{Signal: *res.Signal}
		}
		return &sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_Exited{Exited: ex}}, "exited"
	case ownsand.ExecResultFailed:
		return &sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_Failed{Failed: &sandrunv1.DevSessionExecFailed{
			Reason: sandrunv1.ExecFailureReason(res.FailureReason), Message: res.FailureMessage}}}, "failed"
	}
	reason := strings.TrimPrefix(res.EndReason.String(), "SANDBOX_END_REASON_")
	return &sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_SandboxEnded{SandboxEnded: &sandrunv1.DevSessionExecSandboxEnded{Reason: reason}}}, "sandbox_ended"
}

type responseControllerKey struct{}

func (h *Handler) startRelay(ctx context.Context, start time.Time) (context.Context, context.CancelFunc, error) {
	rotateAt := start.Add(h.timing.StreamLimit)
	controller, ok := ctx.Value(responseControllerKey{}).(*http.ResponseController)
	if !ok {
		return nil, nil, h.svc.Internal(ctx, "set dev session response write deadline", http.ErrNotSupported)
	}
	// Keep the deadline active after the RPC method returns. Connect still
	// has to write and flush the final status.
	if err := controller.SetWriteDeadline(rotateAt.Add(h.timing.WriteGracePeriod)); err != nil {
		return nil, nil, h.svc.Internal(ctx, "set dev session response write deadline", err)
	}
	streamCtx, cancel := context.WithDeadline(ctx, rotateAt)
	return streamCtx, cancel, nil
}

// relayExec forwards exec events and sends a keepalive when the stream is
// idle for KeepaliveInterval. At StreamLimit the OwnSand relay stops. A
// client that keeps reading gets an OK status without a final event and
// continues with AttachDevSessionExec.
func (h *Handler) relayExec(ctx context.Context, sess *session, execID string, offsets execOffsets, send func(*sandrunv1.DevSessionExecEvent) error,
	run func(ctx context.Context, onEvent func(ownsand.ExecEvent) error) (*ownsand.ExecResult, error)) error {
	start := time.Now()
	streamCtx, cancel, err := h.startRelay(ctx, start)
	if err != nil {
		return err
	}
	defer cancel()
	sender := &execSender{send: send, last: time.Now()}
	var sendErr error
	var once sync.Once
	fail := func(err error) {
		once.Do(func() { sendErr = err })
		cancel()
	}
	done := make(chan struct{})
	var keepalives sync.WaitGroup
	keepalives.Add(1)
	go func() {
		defer keepalives.Done()
		t := time.NewTicker(min(h.timing.KeepaliveInterval/2, time.Second*5))
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-streamCtx.Done():
				return
			case <-t.C:
				if sender.idle() >= h.timing.KeepaliveInterval {
					if err := sender.Send(&sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_Keepalive{Keepalive: &sandrunv1.DevSessionExecKeepalive{}}}); err != nil {
						fail(err)
						return
					}
				}
			}
		}
	}()
	res, err := run(streamCtx, func(ev ownsand.ExecEvent) error {
		if out := offsets.convertEvent(ev); out != nil {
			if err := sender.Send(out); err != nil {
				fail(err)
				return err
			}
		}
		return nil
	})
	close(done)
	// The stream must not be written after the handler returns.
	keepalives.Wait()
	log := h.svc.Logger.With(slog.String("organization_id", sess.req.OrganizationID), slog.String("request_id", sess.req.ID),
		slog.String("runner_id", sess.req.RunnerID), slog.String("exec_id", execID))
	if sendErr != nil {
		return sendErr
	}
	if err != nil {
		if ctx.Err() == nil && errors.Is(streamCtx.Err(), context.DeadlineExceeded) {
			log.InfoContext(ctx, "dev_session_exec_stream_rotated", slog.Int64("duration_ms", time.Since(start).Milliseconds()))
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.WarnContext(ctx, "dev_session_exec_relay_failed", slog.String("code", ownsandclient.Code(err)))
		return ownsandError(err)
	}
	final, result := resultEvent(res)
	h.e.Metrics().DevSessionExec(strings.TrimPrefix(sess.guestOS.String(), "GUEST_OS_"), result)
	attrs := []any{slog.String("result", result), slog.Int64("duration_ms", time.Since(start).Milliseconds()), slog.Bool("timed_out", res.TimedOut)}
	if res.ExitCode != nil {
		attrs = append(attrs, slog.Int64("exit_code", *res.ExitCode))
	}
	if res.Signal != nil {
		attrs = append(attrs, slog.Int("signal", int(*res.Signal)))
	}
	if res.Kind == ownsand.ExecResultFailed {
		attrs = append(attrs, slog.String("failure_reason", res.FailureReason.String()))
	}
	log.InfoContext(ctx, "dev_session_exec_ended", attrs...)
	return sender.Send(final)
}

// ExecDevSession runs one command in a running dev session. The command,
// its environment and its output are never logged, stored or audited.
func (h *Handler) ExecDevSession(ctx context.Context, req *connect.Request[sandrunv1.ExecDevSessionRequest], stream *connect.ServerStream[sandrunv1.ExecDevSessionResponse]) error {
	m := req.Msg
	sess, _, err := h.operable(ctx, m.GetOrganizationId(), m.GetDevSessionId())
	if err != nil {
		return err
	}
	execID, err := lowerUUIDv7("exec_id", m.GetExecId())
	if err != nil {
		return err
	}
	if len(m.GetCommand()) == 0 || m.GetCommand()[0] == "" {
		return api.ErrInvalidf("invalid command: the program is required")
	}
	wd, err := resolvePath(sess.guestOS, sess.workspace, m.GetWorkingDirectory())
	if err != nil {
		return err
	}
	var timeout time.Duration
	if t := m.GetTimeout(); t != nil {
		if t.CheckValid() != nil || t.AsDuration() <= 0 {
			return api.ErrInvalidf("invalid timeout: must be greater than zero")
		}
		timeout = t.AsDuration()
	}
	client, _, err := h.e.Links().Client(ctx, sess.req.OrganizationID)
	if err != nil {
		return h.svc.Internal(ctx, "exec dev session", err)
	}
	return h.relayExec(ctx, sess, execID, execOffsets{}, func(ev *sandrunv1.DevSessionExecEvent) error {
		return stream.Send(&sandrunv1.ExecDevSessionResponse{Event: ev})
	}, func(ctx context.Context, onEvent func(ownsand.ExecEvent) error) (*ownsand.ExecResult, error) {
		return client.Exec(ctx, sess.runner.SandboxID, ownsand.ExecParams{
			ExecID: execID, Command: m.GetCommand(), Env: m.GetEnv(), WorkingDirectory: wd, Timeout: timeout, OnEvent: onEvent,
		})
	})
}

// AttachDevSessionExec continues the event stream of an exec.
func (h *Handler) AttachDevSessionExec(ctx context.Context, req *connect.Request[sandrunv1.AttachDevSessionExecRequest], stream *connect.ServerStream[sandrunv1.AttachDevSessionExecResponse]) error {
	m := req.Msg
	sess, _, err := h.operable(ctx, m.GetOrganizationId(), m.GetDevSessionId())
	if err != nil {
		return err
	}
	execID, err := lowerUUIDv7("exec_id", m.GetExecId())
	if err != nil {
		return err
	}
	offsets := execOffsets{stdout: m.GetStdoutOffset(), stderr: m.GetStderrOffset()}
	if offsets.stdout < 0 || offsets.stderr < 0 {
		return api.ErrInvalidf("invalid offsets: must not be negative")
	}
	client, _, err := h.e.Links().Client(ctx, sess.req.OrganizationID)
	if err != nil {
		return h.svc.Internal(ctx, "attach dev session exec", err)
	}
	return h.relayExec(ctx, sess, execID, offsets, func(ev *sandrunv1.DevSessionExecEvent) error {
		return stream.Send(&sandrunv1.AttachDevSessionExecResponse{Event: ev})
	}, func(ctx context.Context, onEvent func(ownsand.ExecEvent) error) (*ownsand.ExecResult, error) {
		return client.AttachExec(ctx, sess.runner.SandboxID, execID, ownsand.AttachParams{StdoutOffset: offsets.stdout, StderrOffset: offsets.stderr, OnEvent: onEvent})
	})
}

// chunkWriter sends what the OwnSand read delivers as chunks of at most 1
// MiB with their offsets.
type chunkWriter struct {
	send    func(*sandrunv1.ReadDevSessionFileResponse) error
	offset  int64
	bytes   int64
	sendErr error
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		c := p[:min(len(p), maxChunk)]
		if err := w.send(&sandrunv1.ReadDevSessionFileResponse{Event: &sandrunv1.ReadDevSessionFileResponse_Chunk{Chunk: &sandrunv1.DevSessionFileChunk{Offset: w.offset, Data: c}}}); err != nil {
			w.sendErr = err
			return n, err
		}
		w.offset += int64(len(c))
		w.bytes += int64(len(c))
		n += len(c)
		p = p[len(c):]
	}
	return n, nil
}

// ReadDevSessionFile streams a guest file from an offset. If the relay
// reaches StreamLimit before reading the entire file, a client that keeps
// reading gets an OK status without an end message. The client continues
// with the same read_id from its next offset.
func (h *Handler) ReadDevSessionFile(ctx context.Context, req *connect.Request[sandrunv1.ReadDevSessionFileRequest], stream *connect.ServerStream[sandrunv1.ReadDevSessionFileResponse]) error {
	m := req.Msg
	sess, _, err := h.operable(ctx, m.GetOrganizationId(), m.GetDevSessionId())
	if err != nil {
		return err
	}
	readID, err := lowerUUIDv7("read_id", m.GetReadId())
	if err != nil {
		return err
	}
	if m.GetOffset() < 0 {
		return api.ErrInvalidf("invalid offset: must not be negative")
	}
	if m.GetPath() == "" {
		return api.ErrInvalidf("invalid path: is required")
	}
	path, err := resolvePath(sess.guestOS, sess.workspace, m.GetPath())
	if err != nil {
		return err
	}
	client, _, err := h.e.Links().Client(ctx, sess.req.OrganizationID)
	if err != nil {
		return h.svc.Internal(ctx, "read dev session file", err)
	}
	start := time.Now()
	streamCtx, cancel, err := h.startRelay(ctx, start)
	if err != nil {
		return err
	}
	defer cancel()
	w := &chunkWriter{send: stream.Send, offset: m.GetOffset()}
	size, err := client.ReadFile(streamCtx, sess.runner.SandboxID, ownsand.FileReadParams{
		ReadID: readID, Path: path, BindingSalt: h.bindingSalt(sess.runner.SandboxID, readID), Offset: m.GetOffset()}, w)
	h.e.Metrics().TransferBytes("download", strings.TrimPrefix(sess.guestOS.String(), "GUEST_OS_"), w.bytes)
	log := h.svc.Logger.With(slog.String("organization_id", sess.req.OrganizationID), slog.String("request_id", sess.req.ID), slog.String("read_id", readID))
	if w.sendErr != nil {
		return w.sendErr
	}
	if err != nil {
		if ctx.Err() == nil && errors.Is(streamCtx.Err(), context.DeadlineExceeded) {
			log.InfoContext(ctx, "dev_session_read_stream_rotated", slog.Int64("bytes", w.bytes), slog.Int64("duration_ms", time.Since(start).Milliseconds()))
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.WarnContext(ctx, "dev_session_read_failed", slog.String("code", ownsandclient.Code(err)), slog.Int64("bytes", w.bytes))
		return fileTransferError(err)
	}
	log.InfoContext(ctx, "dev_session_file_read", slog.Int64("bytes", w.bytes), slog.Int64("size_bytes", size), slog.Int64("duration_ms", time.Since(start).Milliseconds()))
	return stream.Send(&sandrunv1.ReadDevSessionFileResponse{Event: &sandrunv1.ReadDevSessionFileResponse_End{End: &sandrunv1.DevSessionFileEnd{SizeBytes: size}}})
}

func fileTransferError(err error) error {
	var ce *connect.Error
	if errors.As(err, &ce) && ce.Code() == connect.CodePermissionDenied &&
		ce.Message() == "request does not match the recorded operation" {
		return api.ErrPrecondition("Restart the file transfer from the beginning with a new operation ID.")
	}
	return ownsandError(err)
}

// writes keeps the OwnSand FileWrite of each write_id on this instance
// between the unary WriteDevSessionFile calls of one file. An entry that
// receives no chunk for writeIdle is dropped (the guest helper drops the
// temporary file of a write after 15 minutes without a chunk). After a
// deployment handover, the new instance opens a new FileWrite with the same
// write_id and the same derived binding salt, so the write continues.
type writes struct {
	mu      sync.Mutex
	entries map[string]*writeEntry
}

type writeEntry struct {
	busy     chan struct{} // Holds one token while a chunk or the expiry callback owns the entry.
	fw       ownsandclient.FileWrite
	path     string
	deadline time.Time
	timer    *time.Timer
}

const writeIdle = 20 * time.Minute

func newWrites() *writes { return &writes{entries: map[string]*writeEntry{}} }

// acquire returns the current entry with exclusive ownership until release.
func (w *writes) acquire(ctx context.Context, key, path string, open func() (ownsandclient.FileWrite, error)) (*writeEntry, error) {
	for {
		e, err := w.getOrOpen(ctx, key, path, open)
		if err != nil {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case e.busy <- struct{}{}:
		}
		w.mu.Lock()
		current := w.entries[key] == e
		w.mu.Unlock()
		// Cancellation can race with the entry becoming available.
		if err := ctx.Err(); err != nil {
			<-e.busy
			return nil, err
		}
		if !current {
			// Another chunk or the timer removed this entry while we waited.
			<-e.busy
			continue
		}
		if !time.Now().Before(e.deadline) {
			w.drop(key, e)
			<-e.busy
			continue
		}
		if e.path != path {
			<-e.busy
			return nil, api.ErrInvalidf("invalid path: every chunk of a write_id must name the same path")
		}
		return e, nil
	}
}

func (w *writes) getOrOpen(ctx context.Context, key, path string, open func() (ownsandclient.FileWrite, error)) (*writeEntry, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if e := w.entries[key]; e != nil {
		return e, nil
	}
	fw, err := open()
	if err != nil {
		return nil, err
	}
	e := &writeEntry{busy: make(chan struct{}, 1), fw: fw, path: path, deadline: time.Now().Add(writeIdle)}
	w.entries[key] = e
	e.timer = time.AfterFunc(time.Until(e.deadline), func() { w.expire(key, e) })
	return e, nil
}

func (w *writes) expire(key string, e *writeEntry) {
	e.busy <- struct{}{}
	defer func() { <-e.busy }()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.entries[key] != e {
		return
	}
	if remaining := time.Until(e.deadline); remaining > 0 {
		// A successful chunk moved the deadline after this timer was set.
		e.timer.Reset(remaining)
		return
	}
	delete(w.entries, key)
}

// drop removes the current entry while the caller owns it.
func (w *writes) drop(key string, e *writeEntry) {
	w.mu.Lock()
	defer w.mu.Unlock()
	e.timer.Stop()
	delete(w.entries, key)
}

func (w *writes) release(key string, e *writeEntry, finished bool, err error) {
	defer func() { <-e.busy }()
	if finished || connect.CodeOf(err) == connect.CodeNotFound {
		w.drop(key, e)
	} else if err == nil {
		e.deadline = time.Now().Add(writeIdle)
	}
}

// WriteDevSessionFile writes one chunk of a guest file. Every chunk of a
// file uses one write_id; the final chunk finishes the write, which needs
// the written chunks to cover exactly [0, offset + len(data)).
func (h *Handler) WriteDevSessionFile(ctx context.Context, req *connect.Request[sandrunv1.WriteDevSessionFileRequest]) (*connect.Response[sandrunv1.WriteDevSessionFileResponse], error) {
	m := req.Msg
	sess, _, err := h.operable(ctx, m.GetOrganizationId(), m.GetDevSessionId())
	if err != nil {
		return nil, err
	}
	writeID, err := lowerUUIDv7("write_id", m.GetWriteId())
	if err != nil {
		return nil, err
	}
	if m.GetOffset() < 0 {
		return nil, api.ErrInvalidf("invalid offset: must not be negative")
	}
	chunkSize := len(m.GetData())
	if chunkSize > maxChunk {
		return nil, api.ErrInvalidf("invalid data: must be at most 1 MiB")
	}
	if m.GetOffset() > math.MaxInt64-int64(chunkSize) {
		return nil, api.ErrInvalidf("invalid offset: the chunk end offset exceeds the maximum int64 value")
	}
	if m.GetMode() > 0o7777 {
		return nil, api.ErrInvalidf("invalid mode: must be at most 07777")
	}
	if m.GetPath() == "" {
		return nil, api.ErrInvalidf("invalid path: is required")
	}
	path, err := resolvePath(sess.guestOS, sess.workspace, m.GetPath())
	if err != nil {
		return nil, err
	}
	client, _, err := h.e.Links().Client(ctx, sess.req.OrganizationID)
	if err != nil {
		return nil, h.svc.Internal(ctx, "write dev session file", err)
	}
	sandboxID := sess.runner.SandboxID
	key := sandboxID + "/" + writeID
	entry, err := h.writes.acquire(ctx, key, path, func() (ownsandclient.FileWrite, error) {
		fw, err := client.OpenFileWrite(sandboxID, ownsand.FileWriteParams{WriteID: writeID, Path: path, BindingSalt: h.bindingSalt(sandboxID, writeID)})
		if err != nil {
			return nil, fileTransferError(err)
		}
		return fw, nil
	})
	if err != nil {
		return nil, err
	}
	start := time.Now()
	var res *sandrunv1.WriteDevSessionFileResponse
	if m.GetFinal() {
		r, werr := entry.fw.Finish(ctx, m.GetOffset(), m.GetData(), m.GetOffset()+int64(chunkSize), m.GetMode())
		err = werr
		if r != nil {
			res = &sandrunv1.WriteDevSessionFileResponse{WriteId: writeID, Offset: r.GetOffset(), Length: r.GetLength(), Finished: r.GetFinished(), SizeBytes: r.GetSizeBytes()}
		}
	} else {
		r, werr := entry.fw.WriteChunk(ctx, m.GetOffset(), m.GetData())
		err = werr
		if r != nil {
			res = &sandrunv1.WriteDevSessionFileResponse{WriteId: writeID, Offset: r.GetOffset(), Length: r.GetLength()}
		}
	}
	h.writes.release(key, entry, err == nil && res.GetFinished(), err)
	log := h.svc.Logger.With(slog.String("organization_id", sess.req.OrganizationID), slog.String("request_id", sess.req.ID), slog.String("write_id", writeID))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		log.WarnContext(ctx, "dev_session_write_failed", slog.String("code", ownsandclient.Code(err)), slog.Int("size", len(m.GetData())))
		return nil, fileTransferError(err)
	}
	h.e.Metrics().TransferBytes("upload", strings.TrimPrefix(sess.guestOS.String(), "GUEST_OS_"), int64(len(m.GetData())))
	log.InfoContext(ctx, "dev_session_file_write", slog.Int("size", len(m.GetData())), slog.Bool("final", m.GetFinal()), slog.Int64("duration_ms", time.Since(start).Milliseconds()))
	return connect.NewResponse(res), nil
}
