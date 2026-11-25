// SPDX-License-Identifier: Apache-2.0

package ownsand

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"google.golang.org/protobuf/proto"

	"github.com/delinoio/mono/go/ownsand/datapath"
	"github.com/delinoio/mono/go/ownsand/relaybinding"
	"github.com/delinoio/mono/go/ownsand/validate"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// errReadGap reports a chunk that starts after the next undelivered offset.
var errReadGap = errors.New("ownsand: file chunk skipped bytes")

// canonicalFileID matches the control plane's spelling before the SDK binds
// the ID to a relay grant. Request validation still handles invalid IDs.
func canonicalFileID(id string) string {
	if u, err := uuidv7.Parse(id); err == nil {
		return u.String()
	}
	return id
}

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

// FileReadParams describes a read that the caller identifies itself, for a
// program that serves one read in several calls: for example a server that
// relays a read to its own clients and continues it from an offset after its
// client's stream ended. Calls with the same ReadID read the same snapshot of
// the file (see the file transfer contract).
type FileReadParams struct {
	// ReadID is the read_id, a UUID v7 that the caller generated right
	// before the first call of the read. The SDK uses its lowercase form.
	ReadID string
	// Path is the absolute guest path. Every call of one ReadID uses the
	// same path.
	Path string
	// BindingSalt is the binding salt of the read (32 bytes). Every call of
	// one ReadID must use the same salt, because the agent compares the
	// digest of each call with the digest of the first one. Empty generates
	// a salt, which suits a read that one call serves to its end.
	BindingSalt []byte
	// Offset is the first byte that the call delivers.
	Offset int64
}

// ReadFileFrom streams the guest file of p to w from p.Offset and returns
// the file size when the read reaches the end. On an error it returns the
// next undelivered offset. If request validation fails, it returns p.Offset
// for a nonnegative offset, or zero for a negative offset. Like ReadFile,
// it continues an interrupted stream with the same read_id and salt, so w
// never receives a byte twice.
func (s *Sandbox) ReadFileFrom(ctx context.Context, p FileReadParams, w io.Writer) (int64, error) {
	id := canonicalFileID(p.ReadID)
	salt := p.BindingSalt
	if len(salt) == 0 {
		var err error
		if salt, err = relaybinding.NewSalt(); err != nil {
			return 0, err
		}
	}
	base := &ownsandv1.ReadFileRequest{OrganizationId: s.org, SandboxId: s.id, ReadId: id, Path: p.Path, BindingSalt: salt}
	check := proto.Clone(base).(*ownsandv1.ReadFileRequest)
	check.Offset = p.Offset
	if err := validate.ReadFileRequest(check); err != nil {
		return max(p.Offset, 0), err
	}
	op := &readOp{
		s:     s,
		base:  base,
		w:     w,
		next:  p.Offset,
		rec:   newRecovery(s.c.timing.recoveryWindow),
		grant: newRelayGrant(s, ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_READ_FILE, id, relaybinding.ReadFileDigest(base)),
	}
	return op.run(ctx)
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
	// served reports that the agent served the read on this peer-to-peer
	// attempt: it sent a file read event (a keepalive, a chunk or the end) or
	// the accepted frame. It stays false on a relay attempt.
	served  bool
	authErr bool
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
			if (at.accepted || at.served) && isRoutineSessionEnd(err) {
				// A routine session end (idle, the 1-hour expiry) or a
				// retirement (another request's interrupted send tore a
				// frame) of a read that the agent served (it sent a file
				// read event, a keepalive included, or the accepted frame)
				// shows that the operation reached the agent on its new
				// session. It is not a failure, so a slow read can outlive
				// any number of session expiries and retirements. Only
				// failed sessions (a data channel failure, an end or a
				// retirement before the agent served the read) count.
				sessionErrors = 0
				continue
			}
			sessionErrors++
			if sessionErrors > 5 {
				return op.next, op.dpErr(err)
			}
			continue
		}
		// The caller's context stays authoritative. It can end while the
		// attempt fails, so the attempt can report a status that the server
		// answered for the caller's deadline (Connect sends the deadline as a
		// timeout header, and the server's timer can fire first) or the
		// stream reset of the cancellation. classify would take such an
		// error for a terminal failure of the operation.
		if cerr := callerDeadlineEnded(ctx, err, s.c.timing.callerDeadlineSkew); cerr != nil {
			return op.next, cerr
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
		if kind == failDeadline {
			// Only WriteFile retries deadline_exceeded, because its retry
			// is idempotent by write_id. For a read, the status is the
			// control plane's final answer; unless the caller's own
			// deadline caused it (handled above), the operation ends with
			// it.
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
		chunk := data[op.next-off:]
		n, err := op.w.Write(chunk)
		if err != nil {
			// The writer accepted n bytes before it failed: count them, so
			// the returned size matches the bytes that the caller holds.
			// The error ends the operation, so no retry resumes at op.next.
			if n > 0 {
				op.next += int64(min(n, len(chunk)))
			}
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
			at.served = true
			if op.handleEvent(f.GetFileReadEvent(), &at) {
				return at
			}
		case ownsandv1.DataFrameKind_DATA_FRAME_KIND_REQUEST_ACCEPTED:
			at.accepted = true
			at.served = true
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
	// Mode is the file mode, for example 0o644. Zero means 0o644. It must not
	// exceed 0o7777; a larger value makes WriteFile return a validation error
	// for the field finish.mode before it sends any request. Windows guests
	// ignore it.
	Mode uint32
	// OnProgress, when set, receives the acknowledged byte count.
	OnProgress func(written int64)
}

// WriteFile uploads r to the guest file at path in chunks of at most 1 MiB,
// one acknowledged chunk at a time, and finishes the write with the last
// chunk. It keeps the unacknowledged request and sends exactly that request
// again after a failure or a session end, on a new session or on the relay,
// before it reads more input. It generates the write_id when the first request
// is ready to send, after it has read the first chunk and the lookahead, not
// before it reads r, so a slow source does not use up the operation ID
// admission window; the write_id, the binding salt and the grant digest then
// stay the same for every retry and later chunk. It validates the path and the
// mode before it reads r. The target file changes only after finish.
func (s *Sandbox) WriteFile(ctx context.Context, path string, r io.Reader, opts WriteOptions) (int64, error) {
	mode := opts.Mode
	if mode == 0 {
		mode = 0o644
	}
	// Validate the path and the finish fields before the SDK reads r or sends
	// anything. The write_id and the binding salt of the operation do not exist
	// yet: the real write_id is generated only when the first request is ready
	// to send (see below), so these checks use a throwaway ID and a zero salt
	// that are never sent. The first request carries no Finish, so it cannot
	// show a bad mode that only the last request would send; the copy with
	// Finish covers it.
	probeID, err := uuidv7.NewString()
	if err != nil {
		return 0, err
	}
	probe := &ownsandv1.WriteFileRequest{
		OrganizationId: s.org, SandboxId: s.id, WriteId: probeID,
		Path: path, BindingSalt: make([]byte, relaybinding.SaltSize),
	}
	if err := validate.WriteFileRequest(probe, true); err != nil {
		return 0, err
	}
	probe.Finish = &ownsandv1.WriteFileFinish{Mode: mode}
	if err := validate.WriteFileRequest(probe, true); err != nil {
		return 0, err
	}
	var (
		op   *writeOp
		salt []byte
	)

	buf := make([]byte, datapath.MaxFileChunkSize)
	nextBuf := make([]byte, datapath.MaxFileChunkSize)
	// Observe cancellation before every call to the source's Read, so a
	// canceled context never enters r.Read (readChunk repeats the check
	// between the Read calls of one chunk). Nothing is acknowledged before the first
	// chunk, so the offset is 0 here.
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	n, rerr := readChunk(ctx, r, buf)
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
			// Read ahead to learn whether this chunk is the last one. Stop
			// first when ctx is done and report the acknowledged offset.
			if err := ctx.Err(); err != nil {
				return offset, err
			}
			m, nerr = readChunk(ctx, r, nextBuf)
			if nerr != nil && nerr != io.EOF {
				return offset, nerr
			}
			if m == 0 && nerr == io.EOF {
				last = true
			}
		}
		if op == nil {
			// Chunk 1 and the lookahead have been read, so the first request
			// is ready to send. Generate the write_id now: the agent admits an
			// operation ID only within 15 minutes of its UUID v7 timestamp, and
			// a slow source must not use that window up before the first send.
			// The write_id, the salt and the grant digest stay the same for
			// every retry and later chunk of this operation.
			writeID, err := uuidv7.NewString()
			if err != nil {
				return offset, err
			}
			if salt, err = relaybinding.NewSalt(); err != nil {
				return offset, err
			}
			first := &ownsandv1.WriteFileRequest{OrganizationId: s.org, SandboxId: s.id, WriteId: writeID, Path: path, BindingSalt: salt}
			op = &writeOp{
				s:     s,
				id:    writeID,
				grant: newRelayGrant(s, ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_WRITE_FILE, writeID, relaybinding.WriteFileDigest(first)),
			}
		}
		req := &ownsandv1.WriteFileRequest{
			OrganizationId: s.org,
			SandboxId:      s.id,
			WriteId:        op.id,
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

// FileWriteParams describes a write that the caller identifies itself, for
// a program that receives the chunks of one file in several calls (for
// example a server that relays the chunks of its own clients).
type FileWriteParams struct {
	// WriteID is the write_id, a UUID v7 that the caller generated right
	// before the first chunk of the file. The SDK uses its lowercase form.
	WriteID string
	// Path is the absolute guest target path.
	Path string
	// BindingSalt is the binding salt of the write (32 bytes). A program
	// that can open the same write in another process (for example after a
	// restart) passes the same salt there, because the agent compares the
	// digest with the one of the first call. Empty generates a salt.
	BindingSalt []byte
}

// FileWrite is one write that the caller feeds with chunks at offsets. The
// first call through a FileWrite carries the path and the salt; the guest
// helper records the written ranges, and Finish moves the file into place
// when the ranges cover exactly [0, size). Each call sends one chunk and
// retries it after a lost response or a session end with the same request,
// like WriteFile. A FileWrite is safe for concurrent use; its calls are
// serialized.
type FileWrite struct {
	s     *Sandbox
	op    *writeOp
	path  string
	salt  []byte
	mu    sync.Mutex
	first bool
}

// OpenFileWrite returns a FileWrite. It sends nothing; the first chunk
// starts the write in the guest.
func (s *Sandbox) OpenFileWrite(p FileWriteParams) (*FileWrite, error) {
	id := canonicalFileID(p.WriteID)
	salt := p.BindingSalt
	if len(salt) == 0 {
		var err error
		if salt, err = relaybinding.NewSalt(); err != nil {
			return nil, err
		}
	}
	first := &ownsandv1.WriteFileRequest{OrganizationId: s.org, SandboxId: s.id, WriteId: id, Path: p.Path, BindingSalt: salt}
	if err := validate.WriteFileRequest(first, true); err != nil {
		return nil, err
	}
	return &FileWrite{
		s:     s,
		path:  p.Path,
		salt:  salt,
		first: true,
		op: &writeOp{
			s:     s,
			id:    id,
			grant: newRelayGrant(s, ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_WRITE_FILE, id, relaybinding.WriteFileDigest(first)),
		},
	}, nil
}

// WriteChunk writes data (at most 1 MiB) at offset and returns the result of
// the guest helper.
func (fw *FileWrite) WriteChunk(ctx context.Context, offset int64, data []byte) (*ownsandv1.WriteFileResponse, error) {
	return fw.send(ctx, offset, data, nil)
}

// Finish writes the last chunk (data may be empty) at offset and finishes
// the write with the total sizeBytes and the file mode (0 means 0o644).
func (fw *FileWrite) Finish(ctx context.Context, offset int64, data []byte, sizeBytes int64, mode uint32) (*ownsandv1.WriteFileResponse, error) {
	if mode == 0 {
		mode = 0o644
	}
	return fw.send(ctx, offset, data, &ownsandv1.WriteFileFinish{SizeBytes: sizeBytes, Mode: mode})
}

func (fw *FileWrite) send(ctx context.Context, offset int64, data []byte, finish *ownsandv1.WriteFileFinish) (*ownsandv1.WriteFileResponse, error) {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	s := fw.s
	req := &ownsandv1.WriteFileRequest{OrganizationId: s.org, SandboxId: s.id, WriteId: fw.op.id, Offset: offset, Data: data, Finish: finish}
	if fw.first {
		req.Path, req.BindingSalt = fw.path, fw.salt
	}
	if err := validate.WriteFileRequest(req, fw.first); err != nil {
		return nil, err
	}
	res, err := fw.op.send(ctx, req)
	if err != nil {
		return nil, err
	}
	fw.first = false
	if res.GetOffset() != offset || res.GetLength() != int64(len(data)) {
		return nil, fw.op.dpErr(fmt.Errorf("ownsand: write result for offset %d length %d, sent offset %d length %d", res.GetOffset(), res.GetLength(), offset, len(data)))
	}
	if finish != nil && !res.GetFinished() {
		return nil, fw.op.dpErr(errors.New("ownsand: the last chunk was acknowledged without finish"))
	}
	return res, nil
}

// readChunk fills buf from r. It returns io.EOF, also together with n > 0,
// only when r reports a clean end of input, and returns every other error from
// r unchanged. Unlike io.ReadFull, it never turns a short read into
// io.ErrUnexpectedEOF, so a source that reports io.ErrUnexpectedEOF (for
// example a truncated compressed stream) is not mistaken for the end of input.
//
// It checks ctx before every call to r.Read, not only once per chunk, because
// a chunk can need several Read calls and a short read may be the one that
// cancels ctx. When ctx is done it returns the bytes already buffered together
// with ctx.Err(); the callers discard those bytes because they are not
// acknowledged.
func readChunk(ctx context.Context, r io.Reader, buf []byte) (int, error) {
	const maxEmptyReads = 100
	n, empty := 0, 0
	for n < len(buf) {
		if err := ctx.Err(); err != nil {
			return n, err
		}
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
		// The caller's context stays authoritative. It can end while the
		// attempt fails, so the attempt can report a status that the server
		// answered for the caller's deadline (Connect sends the deadline as a
		// timeout header, and the server's timer can fire first) or the
		// stream reset of the cancellation. classify would take such an
		// error for a terminal failure of the operation.
		if cerr := callerDeadlineEnded(ctx, err, t.callerDeadlineSkew); cerr != nil {
			return nil, cerr
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
	actx, cancel := context.WithTimeoutCause(ctx, op.s.c.timing.writeAttemptDeadline, errAttemptDeadline)
	defer cancel()
	attemptErr := func(err error) error {
		if callerErr := ctx.Err(); callerErr != nil {
			return callerErr
		}
		if context.Cause(actx) == errAttemptDeadline {
			return errAttemptDeadline
		}
		return err
	}

	st, err := sess.openStream(actx, proto.Clone(req))
	if err != nil {
		return nil, attemptErr(err)
	}
	defer st.close()
	for {
		f, err := st.recv(actx)
		if err != nil {
			return nil, attemptErr(err)
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
	if rec.streamBudget() < 0 {
		// The recovery window ended while the grant was renewed: start no
		// retry. The initial attempt has no active window, so it never takes
		// this path, and a retry that starts in time may still finish after
		// the window. Return the error as an authorization-class (terminal)
		// result so that send does not classify, back off or retry it.
		return nil, true, rec.deadlineErr()
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
