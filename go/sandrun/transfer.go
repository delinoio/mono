// SPDX-License-Identifier: Apache-2.0

package sandrun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/uuidv7"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
)

// ChunkSize is the largest chunk of WriteDevSessionFile and
// ReadDevSessionFile: 1 MiB.
const ChunkSize = 1 << 20

// TransferPhase is the phase of a sync or download.
type TransferPhase int

const (
	// TransferPreparing: reading the dev session, listing and hashing local
	// files, reading the manifest, or packing the guest directory.
	TransferPreparing TransferPhase = iota + 1
	// TransferUploading: sending the archive.
	TransferUploading
	// TransferDownloading: receiving the file or archive.
	TransferDownloading
	// TransferApplying: extracting, deleting and writing the manifest in
	// the guest, or extracting and moving the download locally.
	TransferApplying
	// TransferDone: the transfer ended successfully.
	TransferDone
)

// String returns the stable identifier of p.
func (p TransferPhase) String() string {
	switch p {
	case TransferPreparing:
		return "preparing"
	case TransferUploading:
		return "uploading"
	case TransferDownloading:
		return "downloading"
	case TransferApplying:
		return "applying"
	case TransferDone:
		return "done"
	}
	return fmt.Sprintf("TransferPhase(%d)", int(p))
}

// TransferProgress reports the progress of a sync or download.
type TransferProgress struct {
	Phase TransferPhase
	// BytesDone and BytesTotal count the bytes of the archive or file on
	// the wire. BytesTotal is zero until it is known.
	BytesDone  int64
	BytesTotal int64
}

// writeGuestFile uploads r to the guest path with WriteDevSessionFile in
// chunks of at most 1 MiB that share one write_id; the last chunk has final
// set. An empty r writes an empty file. Transient failures of a chunk are
// retried with the same write_id and offset. progress receives the bytes
// written so far.
func (c *Client) writeGuestFile(ctx context.Context, s *sandrunv1.DevSession, path string, mode uint32, r io.Reader, progress func(int64)) (int64, error) {
	writeID, err := uuidv7.NewString()
	if err != nil {
		return 0, err
	}
	cur := make([]byte, ChunkSize)
	nxt := make([]byte, ChunkSize)
	n, err := io.ReadFull(r, cur)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return 0, err
	}
	cur = cur[:n]
	var offset int64
	for {
		final := len(cur) < ChunkSize
		var m int
		if !final {
			m, err = io.ReadFull(r, nxt[:ChunkSize])
			if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
				return offset, err
			}
			final = m == 0
		}
		req := &sandrunv1.WriteDevSessionFileRequest{
			OrganizationId: s.GetOrganizationId(),
			DevSessionId:   s.GetDevSessionId(),
			WriteId:        writeID,
			Path:           path,
			Offset:         offset,
			Data:           cur,
			Final:          final,
		}
		if final {
			req.Mode = mode
		}
		if err := c.retry(ctx, "write-file", writeID, func() error {
			_, err := c.devSessions.WriteDevSessionFile(ctx, connect.NewRequest(req))
			return err
		}); err != nil {
			return offset, err
		}
		offset += int64(len(cur))
		if progress != nil {
			progress(offset)
		}
		if final {
			return offset, nil
		}
		cur, nxt = nxt[:m], cur[:cap(cur)]
	}
}

// readGuestFile streams the guest file at path into w with
// ReadDevSessionFile and returns its size. It stops with an error when the
// file is larger than limit bytes. After a transient failure or a stream
// that ends without its end message, it continues from the last offset with
// the same read_id, because the guest serves every call of a read_id from
// one snapshot of the file: a new read_id would join the start of one
// version of the file to the rest of a later version. progress receives the
// bytes read so far.
func (c *Client) readGuestFile(ctx context.Context, s *sandrunv1.DevSession, path string, limit int64, w io.Writer, progress func(int64)) (int64, error) {
	var offset int64
	bo := backoff{min: c.timing.backoffMin, max: c.timing.backoffMax}
	lastProgress := time.Now()
	readID, err := uuidv7.NewString()
	if err != nil {
		return 0, err
	}
	for attempt := 0; ; attempt++ {
		stream, err := c.devSessions.ReadDevSessionFile(ctx, connect.NewRequest(&sandrunv1.ReadDevSessionFileRequest{
			OrganizationId: s.GetOrganizationId(),
			DevSessionId:   s.GetDevSessionId(),
			ReadId:         readID,
			Path:           path,
			Offset:         offset,
		}))
		if err == nil {
			for stream.Receive() {
				msg := stream.Msg()
				if end := msg.GetEnd(); end != nil {
					_ = stream.Close()
					if end.GetSizeBytes() != offset {
						return offset, errorf("read of the guest file ended at byte %d of %d", offset, end.GetSizeBytes())
					}
					return offset, nil
				}
				ch := msg.GetChunk()
				if ch == nil {
					continue
				}
				if ch.GetOffset() != offset {
					_ = stream.Close()
					return offset, errorf("read of the guest file returned offset %d, want %d", ch.GetOffset(), offset)
				}
				if offset+int64(len(ch.GetData())) > limit {
					_ = stream.Close()
					return offset, &LimitError{Operation: "download", FileLimit: limit, TotalLimit: limit, Paths: []PathSize{{Path: path, Size: offset + int64(len(ch.GetData()))}}}
				}
				if _, err := w.Write(ch.GetData()); err != nil {
					_ = stream.Close()
					return offset, err
				}
				offset += int64(len(ch.GetData()))
				lastProgress = time.Now()
				bo.reset()
				if progress != nil {
					progress(offset)
				}
			}
			err = stream.Err()
			_ = stream.Close()
			if err == nil {
				err = errors.New("the read stream ended without its end message")
			}
		}
		if !transient(ctx, err) || time.Since(lastProgress) > c.timing.retryBudget {
			return offset, err
		}
		delay := bo.next()
		c.logger.DebugContext(ctx, "SandRun read file retries", "read_id", readID, "attempt", attempt+1, "delay", delay, "code", codeText(err))
		if err := sleep(ctx, delay); err != nil {
			return offset, err
		}
	}
}

// retry runs fn until it succeeds, fails with a non-transient error, or the
// retry budget passes.
func (c *Client) retry(ctx context.Context, op, id string, fn func() error) error {
	bo := backoff{min: c.timing.backoffMin, max: c.timing.backoffMax}
	start := time.Now()
	for attempt := 0; ; attempt++ {
		err := fn()
		if err == nil || !transient(ctx, err) || time.Since(start) > c.timing.retryBudget {
			return err
		}
		delay := bo.next()
		c.logger.DebugContext(ctx, "sandrun rpc retries", "op", op, "id", id, "attempt", attempt+1, "delay", delay, "code", codeText(err))
		if err := sleep(ctx, delay); err != nil {
			return err
		}
	}
}

// runningSession reads the dev session and checks that it is RUNNING and
// has a workspace path.
func (c *Client) runningSession(ctx context.Context, org, id string) (*sandrunv1.DevSession, error) {
	res, err := c.devSessions.GetDevSession(ctx, connect.NewRequest(&sandrunv1.GetDevSessionRequest{OrganizationId: org, DevSessionId: id}))
	if err != nil {
		return nil, err
	}
	s := res.Msg.GetDevSession()
	if s.GetState() != sandrunv1.RequestState_REQUEST_STATE_RUNNING {
		return nil, localError(connect.CodeFailedPrecondition, fmt.Errorf("sandrun: the dev session is %s, not running", enumText(s.GetState())))
	}
	if s.GetWorkspacePath() == "" {
		return nil, localError(connect.CodeFailedPrecondition, errNoWorkspace)
	}
	if s.GetOrganizationId() == "" {
		s.OrganizationId = org
	}
	if s.GetDevSessionId() == "" {
		s.DevSessionId = id
	}
	return s, nil
}
