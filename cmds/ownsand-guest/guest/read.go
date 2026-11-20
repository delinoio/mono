package guest

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// SnapshotMethod names how a read snapshot was made.
type SnapshotMethod int

const (
	// SnapshotReflink: a reflink copy, always a point-in-time view.
	SnapshotReflink SnapshotMethod = iota + 1
	// SnapshotCopy: a full copy checked for identity, size and modification time.
	SnapshotCopy
)

// readRecord is the private snapshot of one read_id.
type readRecord struct {
	id     string
	digest []byte

	ready chan struct{} // closed when the snapshot is prepared or failed
	err   error
	path  string // snapshot file
	size  int64
	how   SnapshotMethod

	mu       sync.Mutex
	open     int
	lastCall time.Time
}

func (r *readRecord) expired(now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	select {
	case <-r.ready:
	default:
		return false
	}
	return r.open == 0 && now.Sub(r.lastCall) >= FileOperationRetention
}

func (r *readRecord) remove() {
	if r.path != "" {
		_ = os.Remove(r.path)
	}
}

// serveRead serves one ReadFile call from the snapshot of its read_id.
func (s *Server) serveRead(c *conn, st *stream, req *ownsandv1.ReadFileRequest) error {
	if !datapath.IsUUIDv7(req.GetReadId()) {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("read_id must be a UUID v7"))
	}
	if !isAbsGuestPath(req.GetPath()) {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("path must be an absolute guest path"))
	}
	if req.GetOffset() < 0 {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("offset must not be negative"))
	}
	digest := datapath.ReadDigest(req)

	s.mu.Lock()
	if s.tombstonedLocked(req.GetReadId()) {
		s.mu.Unlock()
		return errNotFound("read")
	}
	r, ok := s.reads[req.GetReadId()]
	if ok && !bytes.Equal(r.digest, digest) {
		s.mu.Unlock()
		return connect.NewError(connect.CodeAlreadyExists, errors.New("read_id is already used by another request"))
	}
	if !ok {
		r = &readRecord{id: req.GetReadId(), digest: digest, ready: make(chan struct{})}
		s.reads[r.id] = r
		go s.prepareSnapshot(r, req.GetPath())
	}
	r.mu.Lock()
	r.open++
	r.mu.Unlock()
	s.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.open--
		r.lastCall = s.cfg.Now()
		r.mu.Unlock()
	}()

	select {
	case <-r.ready:
	case <-st.ctx.Done():
		return st.ctx.Err()
	}
	if r.err != nil {
		// A failed snapshot is forgotten, so a retry with the same read_id
		// makes a new one.
		s.mu.Lock()
		if s.reads[r.id] == r {
			delete(s.reads, r.id)
		}
		s.mu.Unlock()
		return r.err
	}
	if req.GetOffset() > r.size {
		return connect.NewError(connect.CodeOutOfRange, errors.New("offset is past the end of the file"))
	}
	f, err := os.Open(r.path)
	if err != nil {
		return connect.NewError(connect.CodeInternal, errors.New("open read snapshot failed"))
	}
	defer f.Close()
	off := req.GetOffset()
	buf := make([]byte, readChunkSize)
	for off < r.size {
		n, err := f.ReadAt(buf[:min(int64(len(buf)), r.size-off)], off)
		if n > 0 {
			chunk := &ownsandv1.FileChunk{Offset: off, Data: append([]byte(nil), buf[:n]...)}
			if err := c.sendPayload(st, func(f *ownsandv1.DataFrame) {
				f.Payload = &ownsandv1.DataFrame_FileChunk{FileChunk: chunk}
			}); err != nil {
				return err
			}
			off += int64(n)
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return connect.NewError(connect.CodeInternal, errors.New("read snapshot failed"))
		}
		if n == 0 {
			return connect.NewError(connect.CodeInternal, errors.New("read snapshot is shorter than recorded"))
		}
	}
	return c.sendPayload(st, func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_FileReadFinished{FileReadFinished: &ownsandv1.ReadFileResponse_Finished{SizeBytes: r.size}}
	})
}

// prepareSnapshot makes the private snapshot of path for r.
func (s *Server) prepareSnapshot(r *readRecord, path string) {
	defer close(r.ready)
	dir := filepath.Join(s.cfg.StateDir, "reads")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		r.err = connect.NewError(connect.CodeInternal, errors.New("create snapshot directory failed"))
		return
	}
	r.path = filepath.Join(dir, r.id)
	how, size, err := snapshotFile(path, r.path)
	if err != nil {
		_ = os.Remove(r.path)
		r.path = ""
		r.err = err
		s.log.Info("read snapshot failed", "read_id", r.id, "code", connect.CodeOf(err).String())
		return
	}
	r.how, r.size = how, size
	s.log.Info("read snapshot ready", "read_id", r.id, "method", how.String(), "size_bytes", size)
}

func (m SnapshotMethod) String() string {
	switch m {
	case SnapshotReflink:
		return "reflink"
	case SnapshotCopy:
		return "copy"
	default:
		return "unknown"
	}
}

// snapshotFile copies src to dst with a reflink when possible and otherwise
// with a full copy whose source identity, size and modification time must not
// change during the copy.
func snapshotFile(src, dst string) (SnapshotMethod, int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, 0, mapFSError(err)
	}
	defer in.Close()
	before, err := in.Stat()
	if err != nil {
		return 0, 0, mapFSError(err)
	}
	if !before.Mode().IsRegular() {
		return 0, 0, connect.NewError(connect.CodeFailedPrecondition, errors.New("path is not a regular file"))
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, 0, connect.NewError(connect.CodeInternal, errors.New("create read snapshot failed"))
	}
	defer out.Close()
	if reflink(out, in) == nil {
		st, err := out.Stat()
		if err != nil {
			return 0, 0, connect.NewError(connect.CodeInternal, errors.New("stat read snapshot failed"))
		}
		return SnapshotReflink, st.Size(), nil
	}
	n, err := io.Copy(out, in)
	if err != nil {
		return 0, 0, connect.NewError(connect.CodeInternal, errors.New("copy file failed"))
	}
	after, err := in.Stat()
	if err != nil {
		return 0, 0, mapFSError(err)
	}
	current, err := os.Stat(src)
	if err != nil || !sameVersion(before, after) || !os.SameFile(before, current) || n != before.Size() {
		return 0, 0, connect.NewError(connect.CodeAborted, errors.New("file changed while its read snapshot was made"))
	}
	return SnapshotCopy, n, nil
}

func sameVersion(a, b fs.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

func mapFSError(err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return connect.NewError(connect.CodeNotFound, errors.New("file does not exist"))
	case errors.Is(err, fs.ErrPermission):
		return connect.NewError(connect.CodePermissionDenied, errors.New("guest file permission denied"))
	default:
		return connect.NewError(connect.CodeInternal, errors.New("guest file system error"))
	}
}

// isAbsGuestPath reports whether p is an absolute guest path.
func isAbsGuestPath(p string) bool {
	return p != "" && filepath.IsAbs(p)
}
