// Package datapath is the agent's data path: one implementation that serves
// exec and file requests from the peer-to-peer data channel and from the
// relay connection, relay grants and their limits, the relay connection to
// the control plane, and the WebRTC answerer.
package datapath

import (
	"bytes"
	"errors"
	"sync"
	"time"

	"connectrpc.com/connect"
	sdkdp "github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
)

const (
	// ExecRetention is how long the agent keeps an ended exec's output and
	// result for AttachExec.
	ExecRetention = 10 * time.Minute
	// FileRetention is how long a read or write stays known after its last
	// call (the guest helper keeps the snapshot or temporary file as long).
	FileRetention = 15 * time.Minute
	// EndedIDMemory is how long the agent remembers ended or expired
	// operation IDs, measured on its monotonic clock.
	EndedIDMemory = 30 * time.Minute
	// OutputRetention is the kept output per stream of an exec.
	OutputRetention = 8 << 20
)

// opKind is the kind of a recorded operation.
type opKind int

const (
	opExec opKind = iota + 1
	opRead
	opWrite
)

// fileOp is a recorded read or write.
type fileOp struct {
	kind     opKind
	digest   []byte
	lastCall time.Time
	finished bool
	open     int
}

// registry records every operation that the agent accepted, on either
// transport, with its request digest, and remembers ended IDs.
type registry struct {
	now func() time.Time

	mu    sync.Mutex
	execs map[string]*execMirror
	files map[string]*fileOp
	ended map[string]time.Time
}

func newRegistry(now func() time.Time) *registry {
	return &registry{now: now, execs: map[string]*execMirror{}, files: map[string]*fileOp{}, ended: map[string]time.Time{}}
}

// checkNewID applies the expired operation ID rules to an ID that the agent
// does not know: UUID v7, less than 15 minutes old and at most 5 minutes in
// the future, and not remembered as ended. The caller holds r.mu.
func (r *registry) checkNewIDLocked(id string) error {
	if _, ok := r.ended[id]; ok {
		return connect.NewError(connect.CodeNotFound, errors.New("the operation expired or ended; the caller's clock may be behind"))
	}
	u, err := sdkdp.ParseUUIDv7(id)
	if err != nil {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("operation ID must be a UUID v7"))
	}
	switch sdkdp.ClassifyOperationID(u, time.Now()) {
	case sdkdp.OperationIDExpired:
		return connect.NewError(connect.CodeNotFound, errors.New("the operation expired or the caller's clock may be behind"))
	case sdkdp.OperationIDFuture:
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("the operation ID is more than 5 minutes in the future; check the caller's clock"))
	}
	return nil
}

// recordedDigest returns the recorded digest of an operation, if any.
func (r *registry) recordedDigest(kind opKind, id string) ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if kind == opExec {
		if e := r.execs[id]; e != nil {
			return e.digest, true
		}
		return nil, false
	}
	if f := r.files[id]; f != nil && f.kind == kind {
		return f.digest, true
	}
	return nil, false
}

// beginFile records the start of a call for a read or write. first reports
// whether the request carries the bound fields (a ReadFile request, or the
// first WriteFile chunk).
func (r *registry) beginFile(kind opKind, id string, digest []byte, first bool) (*fileOp, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.files[id]
	if f == nil {
		if !first {
			if _, ok := r.ended[id]; ok {
				return nil, connect.NewError(connect.CodeNotFound, errors.New("the write expired or ended"))
			}
			return nil, connect.NewError(connect.CodeNotFound, errors.New("the write is unknown; send the first chunk with its path"))
		}
		if err := r.checkNewIDLocked(id); err != nil {
			return nil, err
		}
		f = &fileOp{kind: kind, digest: digest}
		r.files[id] = f
	} else if f.kind != kind || (first && !bytes.Equal(f.digest, digest)) {
		return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("the operation ID is already used by another request"))
	}
	f.open++
	f.lastCall = r.now()
	return f, nil
}

func (r *registry) endFile(f *fileOp, finished bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f.open--
	f.lastCall = r.now()
	if finished {
		f.finished = true
	}
}

// sweep drops expired operations and ended IDs.
func (r *registry) sweep() {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, e := range r.execs {
		if done, at := e.endedAt(); done && now.Sub(at) >= ExecRetention {
			delete(r.execs, id)
			r.ended[id] = now
		}
	}
	for id, f := range r.files {
		if f.open == 0 && now.Sub(f.lastCall) >= FileRetention {
			delete(r.files, id)
			r.ended[id] = now
		}
	}
	for id, at := range r.ended {
		if now.Sub(at) >= EndedIDMemory {
			delete(r.ended, id)
		}
	}
}

// ring keeps the last limit bytes of an output stream with absolute
// offsets. Byte offset o is stored at index (o - origin) % limit.
type ring struct {
	buf    []byte
	limit  int
	end    int64
	origin int64
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
		pos := int((r.end - r.origin) % int64(r.limit))
		n := min(len(p), r.limit-pos)
		copy(r.buf[pos:], p[:n])
		r.end += int64(n)
		p = p[n:]
	}
}

// skipTo moves the end forward over bytes that the agent never received
// (the guest reported them as truncated); the kept bytes are dropped.
func (r *ring) skipTo(off int64) {
	if off > r.end {
		r.buf = r.buf[:0]
		r.end, r.origin = off, off
	}
}

// first returns the offset of the oldest kept byte.
func (r *ring) first() int64 { return r.end - int64(len(r.buf)) }

// read returns up to n kept bytes from off.
func (r *ring) read(off int64, n int) []byte {
	if off < r.first() || off >= r.end {
		return nil
	}
	n = int(min(int64(n), r.end-off))
	out := make([]byte, 0, n)
	for len(out) < n {
		pos := int((off - r.origin) % int64(r.limit))
		m := min(n-len(out), len(r.buf)-pos)
		out = append(out, r.buf[pos:pos+m]...)
		off += int64(m)
	}
	return out
}
