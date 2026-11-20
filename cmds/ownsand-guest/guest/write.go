package guest

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// span is a written byte range [start, end).
type span struct{ start, end int64 }

// finishInfo identifies a finish call: a repeated finish must match it.
type finishInfo struct {
	size        int64
	mode        uint32
	chunkOffset int64
	chunkDigest [32]byte
	hasChunk    bool
}

// writeRecord is one write_id: a temporary file and its written ranges.
type writeRecord struct {
	id     string
	digest []byte
	target string
	tmp    string

	mu        sync.Mutex
	ranges    []span
	lastChunk time.Time
	finished  bool
	finish    finishInfo
	finishAt  time.Time
}

func (w *writeRecord) expired(now time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.finished {
		return now.Sub(w.finishAt) >= FileOperationRetention
	}
	return now.Sub(w.lastChunk) >= FileOperationRetention
}

func (w *writeRecord) discard() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.finished && w.tmp != "" {
		_ = os.Remove(w.tmp)
	}
}

// contiguous returns the end of the written range that starts at zero.
func (w *writeRecord) contiguous() int64 {
	if len(w.ranges) == 0 || w.ranges[0].start != 0 {
		return 0
	}
	return w.ranges[0].end
}

func (w *writeRecord) addRange(s span) {
	w.ranges = append(w.ranges, s)
	sort.Slice(w.ranges, func(i, j int) bool { return w.ranges[i].start < w.ranges[j].start })
	merged := w.ranges[:1]
	for _, r := range w.ranges[1:] {
		last := &merged[len(merged)-1]
		if r.start <= last.end {
			last.end = max(last.end, r.end)
			continue
		}
		merged = append(merged, r)
	}
	w.ranges = merged
}

// serveWrite writes one chunk of a write_id and, with finish, renames the
// temporary file into place.
func (s *Server) serveWrite(c *conn, st *stream, req *ownsandv1.WriteFileRequest) error {
	res, err := s.writeChunk(req)
	if err != nil {
		return err
	}
	return c.sendPayload(st, func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_FileWriteResult{FileWriteResult: res}
	})
}

func (s *Server) writeChunk(req *ownsandv1.WriteFileRequest) (*ownsandv1.WriteFileResponse, error) {
	if !datapath.IsUUIDv7(req.GetWriteId()) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("write_id must be a UUID v7"))
	}
	chunk := req.GetChunk()
	if chunk != nil && (chunk.GetOffset() < 0 || len(chunk.GetData()) > datapath.MaxFileChunk) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("chunk offset or size is invalid"))
	}
	first := req.GetPath() != ""

	s.mu.Lock()
	if s.tombstonedLocked(req.GetWriteId()) {
		s.mu.Unlock()
		return nil, errNotFound("write")
	}
	w, ok := s.writes[req.GetWriteId()]
	if !ok {
		if !first {
			s.mu.Unlock()
			return nil, errNotFound("write")
		}
		var err error
		w, err = s.newWrite(req)
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		s.writes[w.id] = w
		s.log.Info("write started", "write_id", w.id)
	} else if first && !bytes.Equal(w.digest, datapath.WriteDigest(req)) {
		s.mu.Unlock()
		return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("write_id is already used by another request"))
	}
	s.mu.Unlock()

	w.mu.Lock()
	defer w.mu.Unlock()
	now := s.cfg.Now()
	if w.finished {
		if req.GetFinish() == nil || finishOf(req) != w.finish {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("write already finished with other fields"))
		}
		return &ownsandv1.WriteFileResponse{WrittenBytes: w.finish.size, Finished: true}, nil
	}
	w.lastChunk = now
	if chunk != nil && len(chunk.GetData()) > 0 {
		if err := w.writeAt(chunk.GetOffset(), chunk.GetData()); err != nil {
			return nil, err
		}
	}
	if fin := req.GetFinish(); fin != nil {
		if err := w.complete(finishOf(req)); err != nil {
			return nil, err
		}
		w.finishAt = now
		s.log.Info("write finished", "write_id", w.id, "size_bytes", fin.GetSizeBytes())
		return &ownsandv1.WriteFileResponse{WrittenBytes: w.finish.size, Finished: true}, nil
	}
	return &ownsandv1.WriteFileResponse{WrittenBytes: w.contiguous()}, nil
}

func finishOf(req *ownsandv1.WriteFileRequest) finishInfo {
	fi := finishInfo{size: req.GetFinish().GetSizeBytes(), mode: req.GetFinish().GetMode()}
	if c := req.GetChunk(); c != nil {
		fi.hasChunk = true
		fi.chunkOffset = c.GetOffset()
		fi.chunkDigest = sha256.Sum256(c.GetData())
	}
	return fi
}

// newWrite creates the temporary file next to the target, so the final
// rename stays on one file system. The caller holds s.mu.
func (s *Server) newWrite(req *ownsandv1.WriteFileRequest) (*writeRecord, error) {
	if !isAbsGuestPath(req.GetPath()) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("path must be an absolute guest path"))
	}
	target := filepath.Clean(req.GetPath())
	dir := filepath.Dir(target)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("parent directory does not exist"))
	}
	if fi, err := os.Lstat(target); err == nil && fi.IsDir() {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("path is a directory"))
	}
	tmp := filepath.Join(dir, ".ownsand-write-"+req.GetWriteId())
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return nil, connect.NewError(connect.CodePermissionDenied, errors.New("guest file permission denied"))
		}
		return nil, connect.NewError(connect.CodeInternal, errors.New("create temporary file failed"))
	}
	_ = f.Close()
	return &writeRecord{
		id:        req.GetWriteId(),
		digest:    datapath.WriteDigest(req),
		target:    target,
		tmp:       tmp,
		lastChunk: s.cfg.Now(),
	}, nil
}

// writeAt writes data at off. Bytes that overlap written ranges must equal
// the written bytes. The caller holds w.mu.
func (w *writeRecord) writeAt(off int64, data []byte) error {
	f, err := os.OpenFile(w.tmp, os.O_RDWR, 0)
	if err != nil {
		return connect.NewError(connect.CodeInternal, errors.New("open temporary file failed"))
	}
	defer f.Close()
	end := off + int64(len(data))
	for _, r := range w.ranges {
		lo, hi := max(r.start, off), min(r.end, end)
		if lo >= hi {
			continue
		}
		have := make([]byte, hi-lo)
		if _, err := f.ReadAt(have, lo); err != nil {
			return connect.NewError(connect.CodeInternal, errors.New("read temporary file failed"))
		}
		if !bytes.Equal(have, data[lo-off:hi-off]) {
			return connect.NewError(connect.CodeFailedPrecondition, errors.New("chunk overlaps written bytes with different content"))
		}
	}
	if _, err := f.WriteAt(data, off); err != nil {
		return connect.NewError(connect.CodeInternal, errors.New("write temporary file failed"))
	}
	w.addRange(span{off, end})
	return nil
}

// complete checks that the written ranges cover exactly [0, size) and
// renames the temporary file to the target. The caller holds w.mu.
func (w *writeRecord) complete(fi finishInfo) error {
	if fi.size < 0 {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("size_bytes must not be negative"))
	}
	covered := (fi.size == 0 && len(w.ranges) == 0) ||
		(len(w.ranges) == 1 && w.ranges[0].start == 0 && w.ranges[0].end == fi.size)
	if !covered {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("written ranges do not cover exactly [0, size_bytes)"))
	}
	mode := fs.FileMode(fi.mode) & fs.ModePerm
	if fi.mode == 0 {
		mode = 0o644
	}
	f, err := os.OpenFile(w.tmp, os.O_RDWR, 0)
	if err != nil {
		return connect.NewError(connect.CodeInternal, errors.New("open temporary file failed"))
	}
	syncErr := f.Truncate(fi.size)
	if syncErr == nil {
		syncErr = f.Sync()
	}
	_ = f.Close()
	if syncErr != nil {
		return connect.NewError(connect.CodeInternal, errors.New("flush temporary file failed"))
	}
	if err := os.Chmod(w.tmp, mode); err != nil {
		return connect.NewError(connect.CodeInternal, errors.New("set file mode failed"))
	}
	if err := os.Rename(w.tmp, w.target); err != nil {
		return connect.NewError(connect.CodeInternal, errors.New("rename into place failed"))
	}
	w.finished = true
	w.finish = fi
	return nil
}
