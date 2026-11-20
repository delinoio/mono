package ownsand

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/clock"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// ReadFile streams the guest file at path (an absolute guest path) to w and
// returns the file size. The SDK generates one read ID for the read and
// continues an interrupted read from the next undelivered offset with the
// same read ID, so the caller never receives two versions of a file.
func (s *Sandbox) ReadFile(ctx context.Context, path string, w io.Writer) (int64, error) {
	if path == "" {
		return 0, invalid("path is required")
	}
	op := &readOp{s: s, w: w, req: &ownsandv1.ReadFileRequest{
		OrganizationId: s.org,
		SandboxId:      s.id,
		ReadId:         datapath.NewUUIDv7At(s.c.clk.Now()).String(),
		Path:           path,
		BindingSalt:    datapath.NewBindingSalt(),
	}}
	op.auth = &opAuth{dp: s.dp, kind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_READ_FILE, opID: op.req.GetReadId(), digest: datapath.ReadDigest(op.req)}
	n, err := op.run(ctx)
	if err != nil {
		var de *DataPathError
		if !errors.As(err, &de) {
			err = &DataPathError{Op: "read file", Err: err}
		}
		return n, s.c.redact(err)
	}
	return n, nil
}

// ReadFileBytes reads the guest file at path into memory.
func (s *Sandbox) ReadFileBytes(ctx context.Context, path string) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := s.ReadFile(ctx, path, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type readOp struct {
	s    *Sandbox
	w    io.Writer
	req  *ownsandv1.ReadFileRequest
	auth *opAuth
	next int64
	size int64
	done bool
}

func (op *readOp) request() *ownsandv1.ReadFileRequest {
	r := op.req
	return &ownsandv1.ReadFileRequest{OrganizationId: r.GetOrganizationId(), SandboxId: r.GetSandboxId(), ReadId: r.GetReadId(), Path: r.GetPath(), BindingSalt: r.GetBindingSalt(), Offset: op.next}
}

func (op *readOp) run(ctx context.Context) (int64, error) {
	c := op.s.c
	w := newWindow(c.clk)
	b := newBackoff()
	for {
		if ctx.Err() != nil {
			return op.next, ctx.Err()
		}
		if !w.active {
			b = newBackoff()
		}
		sess, err := op.s.dp.session(ctx)
		if err != nil {
			return op.next, err
		}
		var out attemptOutcome
		if sess != nil {
			out = op.p2pAttempt(ctx, sess, w)
		} else {
			if err := op.auth.ensure(ctx, serverStream, w); err != nil {
				return op.next, err
			}
			out = op.relayAttempt(ctx, w)
			op.auth.streamEnded()
		}
		if out.final {
			return op.size, nil
		}
		if sess != nil && op.s.dp.handleSessionError(sess, out.err) {
			continue
		}
		if out.err == nil {
			if out.accepted || sess != nil {
				w.end()
				continue
			}
			out.err = connect.NewError(connect.CodeUnavailable, errors.New("relay stream ended before the agent accepted it"))
			out.attempt = nil
		}
		f := classifyAttempt(ctx, out.attempt, out.err)
		if !f.retryable(false) {
			// A not_found read ID (expired) and an explicit aborted (the file
			// changed during the snapshot copy) are visible errors.
			return op.next, out.err
		}
		c.log.Debug("read continuation scheduled", slog.String("sandbox_id", op.s.id), slog.String("read_id", op.req.GetReadId()), slog.Int64("offset", op.next), slog.String("code", f.code.String()))
		if sess == nil {
			op.auth.discard()
		}
		w.start()
		if w.expired() {
			return op.next, out.err
		}
		ok, serr := w.waitRetry(ctx, b, f)
		if serr != nil {
			return op.next, serr
		}
		if !ok {
			return op.next, out.err
		}
	}
}

// chunk delivers one complete file chunk; only a complete chunk advances
// the resume offset.
func (op *readOp) chunk(ch *ownsandv1.FileChunk, w *window, out *attemptOutcome) error {
	out.delivered = true
	w.end()
	data, at := ch.GetData(), ch.GetOffset()
	if at > op.next {
		return fmt.Errorf("file chunk at offset %d skips undelivered bytes from offset %d", at, op.next)
	}
	if skip := op.next - at; skip > 0 {
		if skip >= int64(len(data)) {
			return nil
		}
		data = data[skip:]
	}
	if _, err := op.w.Write(data); err != nil {
		return fmt.Errorf("write file contents: %w", err)
	}
	op.next += int64(len(data))
	return nil
}

func (op *readOp) finished(size int64, w *window, out *attemptOutcome) error {
	if size != op.next {
		return fmt.Errorf("read ended at offset %d of a %d-byte file", op.next, size)
	}
	op.size = size
	out.final = true
	w.end()
	return nil
}

func (op *readOp) relayAttempt(ctx context.Context, w *window) attemptOutcome {
	c := op.s.c
	actx, cancel := watchIdle(ctx, c.clk)
	defer cancel(nil)
	out := attemptOutcome{attempt: actx, transport: ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_RELAY}
	req := connect.NewRequest(op.request())
	req.Header().Set(datapath.RelayGrantHeader, op.auth.g.id)
	stream, err := c.relay.ReadFile(actx, req)
	if err != nil {
		out.err = err
		return out
	}
	defer stream.Close()
	for stream.Receive() {
		var herr error
		switch m := stream.Msg().GetMessage().(type) {
		case *ownsandv1.ReadFileResponse_Accepted_:
			out.accepted = true
			w.pause()
		case *ownsandv1.ReadFileResponse_Keepalive_:
		case *ownsandv1.ReadFileResponse_Chunk:
			herr = op.chunk(m.Chunk, w, &out)
		case *ownsandv1.ReadFileResponse_Finished_:
			herr = op.finished(m.Finished.GetSizeBytes(), w, &out)
		}
		if herr != nil {
			out.err = herr
			out.attempt = nil
			return out
		}
		if out.final {
			return out
		}
	}
	out.err = stream.Err()
	return out
}

func (op *readOp) p2pAttempt(ctx context.Context, sess *p2pSession, w *window) attemptOutcome {
	out := attemptOutcome{transport: ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_PEER_TO_PEER}
	st, err := sess.open(func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_FileReadRequest{FileReadRequest: op.request()}
	})
	if err != nil {
		out.err = err
		return out
	}
	defer st.close()
	for {
		f, err := st.recv(ctx)
		if err != nil {
			out.err = err
			return out
		}
		var herr error
		switch p := f.GetPayload().(type) {
		case *ownsandv1.DataFrame_FileChunk:
			herr = op.chunk(p.FileChunk, w, &out)
		case *ownsandv1.DataFrame_FileReadFinished:
			herr = op.finished(p.FileReadFinished.GetSizeBytes(), w, &out)
		case *ownsandv1.DataFrame_Error:
			out.err = frameError(p.Error)
			return out
		}
		if herr != nil {
			out.err = herr
			return out
		}
		if out.final {
			return out
		}
	}
}

// WriteFile uploads r to the guest file at path (an absolute guest path) in
// chunks of at most 1 MiB and finishes the write with the total size and
// mode after the last chunk. The guest helper renames the file into place
// only after the finish, so a failed upload never leaves a partial file. The
// SDK sends one chunk at a time, keeps the request that is not acknowledged
// yet and sends that exact request again after a failure, before it reads
// more input.
func (s *Sandbox) WriteFile(ctx context.Context, path string, r io.Reader, mode uint32) (int64, error) {
	if path == "" {
		return 0, invalid("path is required")
	}
	writeID := datapath.NewUUIDv7At(s.c.clk.Now()).String()
	salt := datapath.NewBindingSalt()
	op := &writeOp{s: s, writeID: writeID}
	op.auth = &opAuth{dp: s.dp, kind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_WRITE_FILE, opID: writeID,
		digest: datapath.Digest(datapath.WriteBinding(salt, s.id, writeID, path))}
	buf := make([]byte, datapath.MaxFileChunk)
	var offset int64
	first := true
	for {
		n, rerr := io.ReadFull(r, buf)
		if rerr != nil && !errors.Is(rerr, io.EOF) && !errors.Is(rerr, io.ErrUnexpectedEOF) {
			return offset, fmt.Errorf("ownsand: read input: %w", rerr)
		}
		eof := rerr != nil
		req := &ownsandv1.WriteFileRequest{OrganizationId: s.org, SandboxId: s.id, WriteId: writeID}
		if first {
			req.Path = path
			req.BindingSalt = salt
		}
		if n > 0 {
			req.Chunk = &ownsandv1.FileChunk{Offset: offset, Data: bytes.Clone(buf[:n])}
			if err := op.send(ctx, req); err != nil {
				return offset, s.c.redact(err)
			}
			first = false
			offset += int64(n)
		}
		if eof {
			fin := &ownsandv1.WriteFileRequest{OrganizationId: s.org, SandboxId: s.id, WriteId: writeID,
				Finish: &ownsandv1.WriteFileFinish{SizeBytes: offset, Mode: mode}}
			if first {
				fin.Path = path
				fin.BindingSalt = salt
			}
			if err := op.send(ctx, fin); err != nil {
				return offset, s.c.redact(err)
			}
			if !op.finished {
				return offset, &DataPathError{Op: "write file", Err: errors.New("the agent did not confirm the finish")}
			}
			return offset, nil
		}
	}
}

// WriteFileBytes writes data to the guest file at path.
func (s *Sandbox) WriteFileBytes(ctx context.Context, path string, data []byte, mode uint32) error {
	_, err := s.WriteFile(ctx, path, bytes.NewReader(data), mode)
	return err
}

type writeOp struct {
	s        *Sandbox
	writeID  string
	auth     *opAuth
	finished bool
}

// send delivers one request until the agent acknowledges it. One 60-second
// recovery budget applies to the request; only an acknowledgement resets it.
func (op *writeOp) send(ctx context.Context, req *ownsandv1.WriteFileRequest) error {
	c := op.s.c
	budget := newWindow(c.clk)
	b := newBackoff()
	for {
		if ctx.Err() != nil {
			return &DataPathError{Op: "write file", Err: ctx.Err()}
		}
		sess, err := op.s.dp.session(ctx)
		if err != nil {
			return &DataPathError{Op: "write file", Err: err}
		}
		var resp *ownsandv1.WriteFileResponse
		var attempt context.Context
		if sess != nil {
			resp, err = op.p2pAttempt(ctx, sess, req)
			if err != nil && op.s.dp.handleSessionError(sess, err) {
				continue
			}
		} else {
			if aerr := op.auth.ensure(ctx, writeCall, budget); aerr != nil {
				return &DataPathError{Op: "write file", Err: aerr}
			}
			actx, cancel := clock.WithTimeout(ctx, c.clk, writeAttemptDeadline, errWriteDeadline)
			attempt = actx
			creq := connect.NewRequest(req)
			creq.Header().Set(datapath.RelayGrantHeader, op.auth.g.id)
			var r *connect.Response[ownsandv1.WriteFileResponse]
			r, err = c.relay.WriteFile(actx, creq)
			cancel()
			op.auth.streamEnded()
			if err == nil {
				resp = r.Msg
			}
		}
		if err == nil {
			if req.GetFinish() != nil {
				op.finished = resp.GetFinished()
			}
			return nil
		}
		f := classifyAttempt(ctx, attempt, err)
		if !f.retryable(true) {
			return &DataPathError{Op: "write file", Err: err}
		}
		c.log.Debug("write chunk retry scheduled", slog.String("sandbox_id", op.s.id), slog.String("write_id", op.writeID), slog.String("code", f.code.String()))
		if sess == nil {
			op.auth.discard()
		}
		budget.start()
		if budget.expired() {
			return &DataPathError{Op: "write file", Err: err}
		}
		ok, serr := budget.waitRetry(ctx, b, f)
		if serr != nil {
			return &DataPathError{Op: "write file", Err: serr}
		}
		if !ok {
			return &DataPathError{Op: "write file", Err: err}
		}
	}
}

func (op *writeOp) p2pAttempt(ctx context.Context, sess *p2pSession, req *ownsandv1.WriteFileRequest) (*ownsandv1.WriteFileResponse, error) {
	st, err := sess.open(func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_FileWriteChunk{FileWriteChunk: req}
	})
	if err != nil {
		return nil, err
	}
	defer st.close()
	actx, cancel := clock.WithTimeout(ctx, op.s.c.clk, writeAttemptDeadline, errWriteDeadline)
	defer cancel()
	for {
		f, err := st.recv(actx)
		if err != nil {
			if ctx.Err() == nil && actx.Err() != nil {
				return nil, connect.NewError(connect.CodeDeadlineExceeded, errWriteDeadline)
			}
			return nil, err
		}
		switch p := f.GetPayload().(type) {
		case *ownsandv1.DataFrame_FileWriteResult:
			return p.FileWriteResult, nil
		case *ownsandv1.DataFrame_Error:
			return nil, frameError(p.Error)
		}
	}
}
