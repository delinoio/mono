// SPDX-License-Identifier: FSL-1.1-ALv2

package devsession

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/go/ownsand"
	"github.com/delinoio/mono/go/uuidv7"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
	"github.com/delinoio/mono/servers/tide-api/internal/ownsandclient"
)

// Binding salts (recorded decision): tide derives the OwnSand binding salt
// of a relayed read or write from its operation ID with the primary key of
// TIDE_SECRET_KEYS, so every call of one read_id or write_id uses the same
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
	send func(*tidev1.DevSessionExecEvent) error
	last time.Time
}

func (s *execSender) Send(ev *tidev1.DevSessionExecEvent) error {
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

// convertEvent copies an OwnSand exec event into the tide schema.
func convertEvent(ev ownsand.ExecEvent) *tidev1.DevSessionExecEvent {
	switch ev.Kind {
	case ownsand.ExecEventStarted:
		return &tidev1.DevSessionExecEvent{Event: &tidev1.DevSessionExecEvent_Started{Started: &tidev1.DevSessionExecStarted{ExecId: ev.ExecID, StartTime: timestamppb.Now()}}}
	case ownsand.ExecEventStdout:
		return &tidev1.DevSessionExecEvent{Event: &tidev1.DevSessionExecEvent_Stdout{Stdout: &tidev1.DevSessionExecOutput{Offset: ev.Offset, Data: ev.Data}}}
	case ownsand.ExecEventStderr:
		return &tidev1.DevSessionExecEvent{Event: &tidev1.DevSessionExecEvent_Stderr{Stderr: &tidev1.DevSessionExecOutput{Offset: ev.Offset, Data: ev.Data}}}
	case ownsand.ExecEventOutputTruncated:
		return &tidev1.DevSessionExecEvent{Event: &tidev1.DevSessionExecEvent_OutputTruncated{OutputTruncated: &tidev1.DevSessionExecOutputTruncated{
			StdoutOffset: ev.MissingStdout.To, StderrOffset: ev.MissingStderr.To}}}
	}
	return nil
}

// resultEvent returns the final event of an exec result and its metric
// result label.
func resultEvent(res *ownsand.ExecResult) (*tidev1.DevSessionExecEvent, string) {
	switch res.Kind {
	case ownsand.ExecResultExited:
		ex := &tidev1.DevSessionExecExited{TimedOut: res.TimedOut, EndTime: timestamppb.Now()}
		switch {
		case res.ExitCode != nil:
			ex.Status = &tidev1.DevSessionExecExited_ExitCode{ExitCode: *res.ExitCode}
		case res.Signal != nil:
			ex.Status = &tidev1.DevSessionExecExited_Signal{Signal: *res.Signal}
		}
		return &tidev1.DevSessionExecEvent{Event: &tidev1.DevSessionExecEvent_Exited{Exited: ex}}, "exited"
	case ownsand.ExecResultFailed:
		return &tidev1.DevSessionExecEvent{Event: &tidev1.DevSessionExecEvent_Failed{Failed: &tidev1.DevSessionExecFailed{
			Reason: tidev1.ExecFailureReason(res.FailureReason), Message: res.FailureMessage}}}, "failed"
	}
	reason := strings.TrimPrefix(res.EndReason.String(), "SANDBOX_END_REASON_")
	return &tidev1.DevSessionExecEvent{Event: &tidev1.DevSessionExecEvent_SandboxEnded{SandboxEnded: &tidev1.DevSessionExecSandboxEnded{Reason: reason}}}, "sandbox_ended"
}

// relayExec forwards the events of one exec stream: it ends the stream
// before StreamLimit with an OK status and no final event (the command keeps
// running; the client continues with AttachDevSessionExec) and sends a
// keepalive when no event went out for KeepaliveInterval.
func (h *Handler) relayExec(ctx context.Context, sess *session, execID string, send func(*tidev1.DevSessionExecEvent) error,
	run func(ctx context.Context, onEvent func(ownsand.ExecEvent) error) (*ownsand.ExecResult, error)) error {
	start := time.Now()
	streamCtx, cancel := context.WithTimeout(ctx, h.timing.StreamLimit)
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
					if err := sender.Send(&tidev1.DevSessionExecEvent{Event: &tidev1.DevSessionExecEvent_Keepalive{Keepalive: &tidev1.DevSessionExecKeepalive{}}}); err != nil {
						fail(err)
						return
					}
				}
			}
		}
	}()
	res, err := run(streamCtx, func(ev ownsand.ExecEvent) error {
		if out := convertEvent(ev); out != nil {
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
func (h *Handler) ExecDevSession(ctx context.Context, req *connect.Request[tidev1.ExecDevSessionRequest], stream *connect.ServerStream[tidev1.ExecDevSessionResponse]) error {
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
	return h.relayExec(ctx, sess, execID, func(ev *tidev1.DevSessionExecEvent) error {
		return stream.Send(&tidev1.ExecDevSessionResponse{Event: ev})
	}, func(ctx context.Context, onEvent func(ownsand.ExecEvent) error) (*ownsand.ExecResult, error) {
		return client.Exec(ctx, sess.runner.SandboxID, ownsand.ExecParams{
			ExecID: execID, Command: m.GetCommand(), Env: m.GetEnv(), WorkingDirectory: wd, Timeout: timeout, OnEvent: onEvent,
		})
	})
}

// AttachDevSessionExec continues the event stream of an exec.
func (h *Handler) AttachDevSessionExec(ctx context.Context, req *connect.Request[tidev1.AttachDevSessionExecRequest], stream *connect.ServerStream[tidev1.AttachDevSessionExecResponse]) error {
	m := req.Msg
	sess, _, err := h.operable(ctx, m.GetOrganizationId(), m.GetDevSessionId())
	if err != nil {
		return err
	}
	execID, err := lowerUUIDv7("exec_id", m.GetExecId())
	if err != nil {
		return err
	}
	if m.GetStdoutOffset() < 0 || m.GetStderrOffset() < 0 {
		return api.ErrInvalidf("invalid offsets: must not be negative")
	}
	client, _, err := h.e.Links().Client(ctx, sess.req.OrganizationID)
	if err != nil {
		return h.svc.Internal(ctx, "attach dev session exec", err)
	}
	return h.relayExec(ctx, sess, execID, func(ev *tidev1.DevSessionExecEvent) error {
		return stream.Send(&tidev1.AttachDevSessionExecResponse{Event: ev})
	}, func(ctx context.Context, onEvent func(ownsand.ExecEvent) error) (*ownsand.ExecResult, error) {
		return client.AttachExec(ctx, sess.runner.SandboxID, execID, ownsand.AttachParams{StdoutOffset: m.GetStdoutOffset(), StderrOffset: m.GetStderrOffset(), OnEvent: onEvent})
	})
}

// chunkWriter sends what the OwnSand read delivers as chunks of at most 1
// MiB with their offsets.
type chunkWriter struct {
	send    func(*tidev1.ReadDevSessionFileResponse) error
	offset  int64
	bytes   int64
	sendErr error
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		c := p[:min(len(p), maxChunk)]
		if err := w.send(&tidev1.ReadDevSessionFileResponse{Event: &tidev1.ReadDevSessionFileResponse_Chunk{Chunk: &tidev1.DevSessionFileChunk{Offset: w.offset, Data: c}}}); err != nil {
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

// ReadDevSessionFile streams a guest file from an offset. The stream ends
// before StreamLimit without the end message when the file is not read to
// its end; the client continues with the same read_id from its next offset.
func (h *Handler) ReadDevSessionFile(ctx context.Context, req *connect.Request[tidev1.ReadDevSessionFileRequest], stream *connect.ServerStream[tidev1.ReadDevSessionFileResponse]) error {
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
	path, err := resolvePath(sess.guestOS, sess.workspace, m.GetPath())
	if err != nil || m.GetPath() == "" {
		return api.ErrInvalidf("invalid path: is required")
	}
	client, _, err := h.e.Links().Client(ctx, sess.req.OrganizationID)
	if err != nil {
		return h.svc.Internal(ctx, "read dev session file", err)
	}
	start := time.Now()
	streamCtx, cancel := context.WithTimeout(ctx, h.timing.StreamLimit)
	defer cancel()
	w := &chunkWriter{send: stream.Send, offset: m.GetOffset()}
	size, err := client.ReadFile(streamCtx, sess.runner.SandboxID, ownsand.FileReadParams{
		ReadID: readID, Path: path, BindingSalt: h.bindingSalt(sess.runner.SandboxID, readID), Offset: m.GetOffset()}, w)
	h.e.Metrics().TransferBytes("download", strings.TrimPrefix(sess.guestOS.String(), "GUEST_OS_"), w.bytes)
	log := h.svc.Logger.With(slog.String("organization_id", sess.req.OrganizationID), slog.String("request_id", sess.req.ID), slog.String("read_id", readID))
	if err != nil {
		if ctx.Err() == nil && errors.Is(streamCtx.Err(), context.DeadlineExceeded) {
			log.InfoContext(ctx, "dev_session_read_stream_rotated", slog.Int64("bytes", w.bytes), slog.Int64("duration_ms", time.Since(start).Milliseconds()))
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if w.sendErr != nil {
			// The client stream failed.
			return w.sendErr
		}
		log.WarnContext(ctx, "dev_session_read_failed", slog.String("code", ownsandclient.Code(err)), slog.Int64("bytes", w.bytes))
		return ownsandError(err)
	}
	log.InfoContext(ctx, "dev_session_file_read", slog.Int64("bytes", w.bytes), slog.Int64("size_bytes", size), slog.Int64("duration_ms", time.Since(start).Milliseconds()))
	return stream.Send(&tidev1.ReadDevSessionFileResponse{Event: &tidev1.ReadDevSessionFileResponse_End{End: &tidev1.DevSessionFileEnd{SizeBytes: size}}})
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
	mu   sync.Mutex
	fw   ownsandclient.FileWrite
	path string
	used time.Time
}

const writeIdle = 20 * time.Minute

func newWrites() *writes { return &writes{entries: map[string]*writeEntry{}} }

func (w *writes) get(key, path string, open func() (ownsandclient.FileWrite, error)) (*writeEntry, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	for k, e := range w.entries {
		if now.Sub(e.used) > writeIdle {
			delete(w.entries, k)
		}
	}
	if e, ok := w.entries[key]; ok {
		if e.path != path {
			return nil, api.ErrInvalidf("invalid path: every chunk of a write_id must name the same path")
		}
		e.used = now
		return e, nil
	}
	fw, err := open()
	if err != nil {
		return nil, err
	}
	e := &writeEntry{fw: fw, path: path, used: now}
	w.entries[key] = e
	return e, nil
}

func (w *writes) drop(key string) {
	w.mu.Lock()
	delete(w.entries, key)
	w.mu.Unlock()
}

// WriteDevSessionFile writes one chunk of a guest file. Every chunk of a
// file uses one write_id; the final chunk finishes the write, which needs
// the written chunks to cover exactly [0, offset + len(data)).
func (h *Handler) WriteDevSessionFile(ctx context.Context, req *connect.Request[tidev1.WriteDevSessionFileRequest]) (*connect.Response[tidev1.WriteDevSessionFileResponse], error) {
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
	if len(m.GetData()) > maxChunk {
		return nil, api.ErrInvalidf("invalid data: must be at most 1 MiB")
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
	entry, err := h.writes.get(key, path, func() (ownsandclient.FileWrite, error) {
		return client.OpenFileWrite(sandboxID, ownsand.FileWriteParams{WriteID: writeID, Path: path, BindingSalt: h.bindingSalt(sandboxID, writeID)})
	})
	if err != nil {
		return nil, h.svc.Internal(ctx, "write dev session file", err)
	}
	start := time.Now()
	entry.mu.Lock()
	var res *tidev1.WriteDevSessionFileResponse
	if m.GetFinal() {
		r, werr := entry.fw.Finish(ctx, m.GetOffset(), m.GetData(), m.GetOffset()+int64(len(m.GetData())), m.GetMode())
		err = werr
		if r != nil {
			res = &tidev1.WriteDevSessionFileResponse{WriteId: writeID, Offset: r.GetOffset(), Length: r.GetLength(), Finished: r.GetFinished(), SizeBytes: r.GetSizeBytes()}
		}
	} else {
		r, werr := entry.fw.WriteChunk(ctx, m.GetOffset(), m.GetData())
		err = werr
		if r != nil {
			res = &tidev1.WriteDevSessionFileResponse{WriteId: writeID, Offset: r.GetOffset(), Length: r.GetLength()}
		}
	}
	entry.mu.Unlock()
	log := h.svc.Logger.With(slog.String("organization_id", sess.req.OrganizationID), slog.String("request_id", sess.req.ID), slog.String("write_id", writeID))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		log.WarnContext(ctx, "dev_session_write_failed", slog.String("code", ownsandclient.Code(err)), slog.Int("size", len(m.GetData())))
		if connect.CodeOf(err) == connect.CodeNotFound {
			// The write expired in the guest; a new write_id starts again.
			h.writes.drop(key)
		}
		return nil, ownsandError(err)
	}
	h.e.Metrics().TransferBytes("upload", strings.TrimPrefix(sess.guestOS.String(), "GUEST_OS_"), int64(len(m.GetData())))
	log.InfoContext(ctx, "dev_session_file_write", slog.Int("size", len(m.GetData())), slog.Bool("final", m.GetFinal()), slog.Int64("duration_ms", time.Since(start).Milliseconds()))
	if res.GetFinished() {
		h.writes.drop(key)
	}
	return connect.NewResponse(res), nil
}
