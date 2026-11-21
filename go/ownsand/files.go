// SPDX-License-Identifier: Apache-2.0

package ownsand

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"

	"github.com/delinoio/mono/go/ownsand/datapath"
	"github.com/delinoio/mono/go/ownsand/relaybinding"
	"github.com/delinoio/mono/go/ownsand/validate"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// errReadGap reports a chunk that starts after the next undelivered offset.
var errReadGap = errors.New("ownsand: file chunk skipped bytes")

// ReadFile streams the guest file at path to w and returns its size. It
// generates one read_id and continues an interrupted read with the same
// read_id, path and salt from the next undelivered offset, so the caller
// never receives a byte twice or two versions of the file.
func (s *Sandbox) ReadFile(ctx context.Context, path string, w io.Writer) (int64, error) {
	readID, err := uuidv7.NewString()
	if err != nil {
		return 0, err
	}
	salt, err := relaybinding.NewSalt()
	if err != nil {
		return 0, err
	}
	base := &ownsandv1.ReadFileRequest{OrganizationId: s.org, SandboxId: s.id, ReadId: readID, Path: path, BindingSalt: salt}
	if err := validate.ReadFileRequest(base); err != nil {
		return 0, err
	}
	op := &readOp{
		s:     s,
		base:  base,
		w:     w,
		rec:   newRecovery(s.c.timing.recoveryWindow),
		grant: newRelayGrant(s, ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_READ_FILE, readID, relaybinding.ReadFileDigest(base)),
	}
	return op.run(ctx)
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
	s     *Sandbox
	base  *ownsandv1.ReadFileRequest
	w     io.Writer
	next  int64
	rec   *recovery
	grant *relayGrant
}

type readAttempt struct {
	done     bool
	size     int64
	err      error
	accepted bool
	authErr  bool
}

func (op *readOp) dpErr(err error) error {
	var cb *callbackError
	if errors.As(err, &cb) {
		return cb.err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &DataPathError{Operation: "read-file", OperationID: op.base.GetReadId(), Err: err}
}

func (op *readOp) request() *ownsandv1.ReadFileRequest {
	req := proto.Clone(op.base).(*ownsandv1.ReadFileRequest)
	req.Offset = op.next
	return req
}

func (op *readOp) run(ctx context.Context) (int64, error) {
	s := op.s
	sessionErrors := 0
	for {
		if err := ctx.Err(); err != nil {
			return op.next, err
		}
		sess, err := s.dp.route(ctx)
		if err != nil {
			return op.next, op.dpErr(err)
		}
		before := op.next
		var at readAttempt
		if sess != nil {
			at = op.p2pAttempt(ctx, sess)
		} else {
			at = op.relayAttempt(ctx)
		}
		if at.done {
			return at.size, nil
		}
		if op.next > before {
			sessionErrors = 0
		}
		err = at.err
		var cb *callbackError
		if errors.As(err, &cb) {
			return op.next, cb.err
		}
		if errors.Is(err, io.EOF) {
			if at.accepted {
				op.rec.reset()
				continue
			}
			err = errNotAccepted
		}
		if sess != nil && s.dp.handleSessionError(sess, err) {
			sessionErrors++
			if sessionErrors > 5 {
				return op.next, op.dpErr(err)
			}
			continue
		}
		if at.authErr {
			return op.next, op.dpErr(err)
		}
		kind, delay := classify(ctx, err)
		if kind == failTerminal {
			// not_found means that the read_id expired; aborted from the
			// guest helper means that the file changed during the copy.
			return op.next, op.dpErr(err)
		}
		op.rec.fail(err)
		if sess == nil && kind != failExhausted {
			op.grant.discard()
		}
		if berr := op.rec.backoff(ctx, s.c.timing, delay); berr != nil {
			return op.next, op.dpErr(berr)
		}
	}
}

// handleEvent applies one file read event.
func (op *readOp) handleEvent(ev *ownsandv1.FileReadEvent, at *readAttempt) bool {
	switch e := ev.GetEvent().(type) {
	case *ownsandv1.FileReadEvent_Accepted:
		at.accepted = true
		op.rec.pauseClock()
	case *ownsandv1.FileReadEvent_Keepalive:
	case *ownsandv1.FileReadEvent_Chunk:
		off, data := e.Chunk.GetOffset(), e.Chunk.GetData()
		end := off + int64(len(data))
		if end <= op.next {
			return false
		}
		if off > op.next {
			at.err = fmt.Errorf("%w: chunk at %d, next offset %d", errReadGap, off, op.next)
			return true
		}
		if _, err := op.w.Write(data[op.next-off:]); err != nil {
			at.err = &callbackError{err: err}
			return true
		}
		op.next = end
		op.rec.reset()
	case *ownsandv1.FileReadEvent_End:
		if e.End.GetSizeBytes() != op.next {
			at.err = fmt.Errorf("ownsand: file read ended at %d bytes but reported %d", op.next, e.End.GetSizeBytes())
			return true
		}
		op.rec.reset()
		at.done = true
		at.size = op.next
		return true
	}
	return false
}

func (op *readOp) p2pAttempt(ctx context.Context, sess *p2pSession) readAttempt {
	var at readAttempt
	st, err := sess.openStream(ctx, op.request())
	if err != nil {
		at.err = err
		return at
	}
	defer st.close()
	for {
		f, err := st.recv(ctx)
		if err != nil {
			at.err = err
			return at
		}
		switch f.GetKind() {
		case ownsandv1.DataFrameKind_DATA_FRAME_KIND_FILE_READ_EVENT:
			if op.handleEvent(f.GetFileReadEvent(), &at) {
				return at
			}
		case ownsandv1.DataFrameKind_DATA_FRAME_KIND_REQUEST_ACCEPTED:
			at.accepted = true
		case ownsandv1.DataFrameKind_DATA_FRAME_KIND_ERROR:
			at.err = errorFromDetail(f.GetError())
			return at
		}
	}
}

func (op *readOp) relayAttempt(ctx context.Context) readAttempt {
	c := op.s.c
	t := c.timing
	op.s.dp.setInUse(ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_RELAY)
	var at readAttempt
	if err := op.grant.ensure(ctx, t.relayStreamLimit+t.streamMargin, op.rec); err != nil {
		at.err, at.authErr = err, true
		return at
	}
	budget := op.rec.streamBudget()
	if budget < 0 {
		// The recovery window ended while the grant was renewed: start no
		// continuation.
		at.err = op.rec.deadlineErr()
		return at
	}
	actx, disarm, stop := c.stallContext(ctx, budget)
	defer stop()
	defer op.grant.streamEnded()
	stream, err := c.relay.ReadFile(actx, relayRequest(op.grant, op.request()))
	if err != nil {
		at.err = streamErr(actx, err, op.rec)
		return at
	}
	defer stream.Close()
	for stream.Receive() {
		done := op.handleEvent(stream.Msg().GetEvent(), &at)
		if at.accepted || !op.rec.inRecovery() {
			// The agent accepted the continuation, or progress ended the
			// window: the recovery budget no longer bounds the stream.
			disarm()
		}
		if done {
			return at
		}
	}
	if err := stream.Err(); err != nil {
		at.err = streamErr(actx, err, op.rec)
		return at
	}
	at.err = io.EOF
	return at
}

// WriteOptions configures a file write.
type WriteOptions struct {
	// Mode is the file mode, for example 0o644. Zero means 0o644. Windows
	// guests ignore it.
	Mode uint32
	// OnProgress, when set, receives the acknowledged byte count.
	OnProgress func(written int64)
}

// WriteFile uploads r to the guest file at path in chunks of at most 1 MiB,
// one acknowledged chunk at a time, and finishes the write with the last
// chunk. It keeps the unacknowledged request and sends exactly that request
// again after a failure or a session end, on a new session or on the relay,
// before it reads more input. The target file changes only after finish.
func (s *Sandbox) WriteFile(ctx context.Context, path string, r io.Reader, opts WriteOptions) (int64, error) {
	writeID, err := uuidv7.NewString()
	if err != nil {
		return 0, err
	}
	salt, err := relaybinding.NewSalt()
	if err != nil {
		return 0, err
	}
	mode := opts.Mode
	if mode == 0 {
		mode = 0o644
	}
	first := &ownsandv1.WriteFileRequest{OrganizationId: s.org, SandboxId: s.id, WriteId: writeID, Path: path, BindingSalt: salt}
	if err := validate.WriteFileRequest(first, true); err != nil {
		return 0, err
	}
	op := &writeOp{
		s:     s,
		id:    writeID,
		grant: newRelayGrant(s, ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_WRITE_FILE, writeID, relaybinding.WriteFileDigest(first)),
	}

	buf := make([]byte, datapath.MaxFileChunkSize)
	nextBuf := make([]byte, datapath.MaxFileChunkSize)
	n, rerr := readChunk(r, buf)
	var offset int64
	isFirst := true
	for {
		if rerr != nil && rerr != io.EOF {
			return offset, rerr
		}
		last := rerr != nil
		var m int
		var nerr error
		if !last {
			// Read ahead to learn whether this chunk is the last one.
			m, nerr = readChunk(r, nextBuf)
			if nerr != nil && nerr != io.EOF {
				return offset, nerr
			}
			if m == 0 && nerr == io.EOF {
				last = true
			}
		}
		req := &ownsandv1.WriteFileRequest{
			OrganizationId: s.org,
			SandboxId:      s.id,
			WriteId:        writeID,
			Offset:         offset,
			Data:           append([]byte(nil), buf[:n]...),
		}
		if isFirst {
			req.Path, req.BindingSalt = path, salt
		}
		if last {
			req.Finish = &ownsandv1.WriteFileFinish{SizeBytes: offset + int64(n), Mode: mode}
		}
		res, err := op.send(ctx, req)
		if err != nil {
			return offset, err
		}
		if res.GetOffset() != offset || res.GetLength() != int64(n) {
			return offset, op.dpErr(fmt.Errorf("ownsand: write result for offset %d length %d, sent offset %d length %d", res.GetOffset(), res.GetLength(), offset, n))
		}
		offset += int64(n)
		isFirst = false
		if opts.OnProgress != nil {
			opts.OnProgress(offset)
		}
		if last {
			if !res.GetFinished() {
				return offset, op.dpErr(errors.New("ownsand: the last chunk was acknowledged without finish"))
			}
			return res.GetSizeBytes(), nil
		}
		buf, nextBuf = nextBuf, buf
		n, rerr = m, nerr
	}
}

// readChunk fills buf from r. It returns io.EOF, also together with n > 0,
// only when r reports a clean end of input, and returns every other error from
// r unchanged. Unlike io.ReadFull, it never turns a short read into
// io.ErrUnexpectedEOF, so a source that reports io.ErrUnexpectedEOF (for
// example a truncated compressed stream) is not mistaken for the end of input.
func readChunk(r io.Reader, buf []byte) (int, error) {
	const maxEmptyReads = 100
	n, empty := 0, 0
	for n < len(buf) {
		m, err := r.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
		if m > 0 {
			empty = 0
		} else if empty++; empty >= maxEmptyReads {
			// Guard against a reader that makes no progress without an error.
			return n, io.ErrNoProgress
		}
	}
	return n, nil
}

// WriteFileBytes writes data to the guest file at path.
func (s *Sandbox) WriteFileBytes(ctx context.Context, path string, data []byte, opts WriteOptions) error {
	_, err := s.WriteFile(ctx, path, bytes.NewReader(data), opts)
	return err
}

type writeOp struct {
	s     *Sandbox
	id    string
	grant *relayGrant
}

func (op *writeOp) dpErr(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &DataPathError{Operation: "write-file", OperationID: op.id, Err: err}
}

// send delivers one chunk or finish request until the agent acknowledges
// it. One 60-second recovery budget applies to the unacknowledged request;
// only an acknowledgement resets it.
func (op *writeOp) send(ctx context.Context, req *ownsandv1.WriteFileRequest) (*ownsandv1.WriteFileResponse, error) {
	s := op.s
	t := s.c.timing
	rec := newRecovery(t.recoveryWindow)
	sessionErrors := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sess, err := s.dp.route(ctx)
		if err != nil {
			return nil, op.dpErr(err)
		}
		var res *ownsandv1.WriteFileResponse
		authErr := false
		if sess != nil {
			res, err = op.p2pAttempt(ctx, sess, req)
		} else {
			res, authErr, err = op.relayAttempt(ctx, req, rec)
		}
		if err == nil {
			return res, nil
		}
		if sess != nil && s.dp.handleSessionError(sess, err) {
			sessionErrors++
			if sessionErrors > 5 {
				return nil, op.dpErr(err)
			}
			continue
		}
		if authErr {
			return nil, op.dpErr(err)
		}
		kind, delay := classify(ctx, err)
		if kind == failTerminal {
			return nil, op.dpErr(err)
		}
		rec.fail(err)
		if sess == nil && kind != failExhausted {
			op.grant.discard()
		}
		if berr := rec.backoff(ctx, t, delay); berr != nil {
			return nil, op.dpErr(berr)
		}
		if rec.expired() {
			return nil, op.dpErr(rec.err())
		}
	}
}

func (op *writeOp) p2pAttempt(ctx context.Context, sess *p2pSession, req *ownsandv1.WriteFileRequest) (*ownsandv1.WriteFileResponse, error) {
	st, err := sess.openStream(ctx, proto.Clone(req))
	if err != nil {
		return nil, err
	}
	defer st.close()
	actx, cancel := context.WithTimeoutCause(ctx, op.s.c.timing.writeAttemptDeadline, errAttemptDeadline)
	defer cancel()
	for {
		f, err := st.recv(actx)
		if err != nil {
			if errors.Is(context.Cause(actx), errAttemptDeadline) && ctx.Err() == nil {
				return nil, errAttemptDeadline
			}
			return nil, err
		}
		switch f.GetKind() {
		case ownsandv1.DataFrameKind_DATA_FRAME_KIND_FILE_WRITE_RESULT:
			return f.GetFileWriteResult(), nil
		case ownsandv1.DataFrameKind_DATA_FRAME_KIND_ERROR:
			return nil, errorFromDetail(f.GetError())
		}
	}
}

func (op *writeOp) relayAttempt(ctx context.Context, req *ownsandv1.WriteFileRequest, rec *recovery) (*ownsandv1.WriteFileResponse, bool, error) {
	c := op.s.c
	t := c.timing
	op.s.dp.setInUse(ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_RELAY)
	if err := op.grant.ensure(ctx, t.writeCallLimit+t.streamMargin, rec); err != nil {
		return nil, true, err
	}
	defer op.grant.streamEnded()
	actx, cancel := context.WithTimeoutCause(ctx, t.writeAttemptDeadline, errAttemptDeadline)
	defer cancel()
	res, err := c.relay.WriteFile(actx, relayRequest(op.grant, proto.Clone(req).(*ownsandv1.WriteFileRequest)))
	if err != nil {
		if errors.Is(context.Cause(actx), errAttemptDeadline) && ctx.Err() == nil {
			return nil, false, errAttemptDeadline
		}
		return nil, false, err
	}
	return res.Msg, false, nil
}
