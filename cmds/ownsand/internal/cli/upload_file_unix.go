// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin

package cli

import (
	"context"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func uploadFileReader(ctx context.Context, f *os.File) (io.Reader, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeNamedPipe == 0 {
		return f, nil
	}

	// The FIFO is already open, so changing its mode preserves the wait for
	// a writer. Use nonblocking reads directly because Go's file poller
	// does not support deadlines on FIFOs on macOS.
	fd := int(f.Fd())
	if err := unix.SetNonblock(fd, true); err != nil {
		return nil, &os.PathError{Op: "set nonblocking", Path: f.Name(), Err: err}
	}
	return &fifoUploadReader{ctx: ctx, fd: fd, path: f.Name()}, nil
}

type fifoUploadReader struct {
	ctx  context.Context
	fd   int
	path string
}

func (r *fifoUploadReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, r.ctx.Err()
	}
	for {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		n, err := unix.Read(r.fd, p)
		switch err {
		case nil:
			if n == 0 {
				return 0, io.EOF
			}
			return n, nil
		case unix.EINTR:
			continue
		case unix.EAGAIN:
			// Wait for data or writer closure. Bound the wait so
			// cancellation works even while the writer stays idle.
			const cancelPollMilliseconds = 50
			fds := [1]unix.PollFd{{Fd: int32(r.fd), Events: unix.POLLIN}}
			if _, err := unix.Poll(fds[:], cancelPollMilliseconds); err != nil && err != unix.EINTR {
				return 0, &os.PathError{Op: "poll", Path: r.path, Err: err}
			}
		default:
			return 0, &os.PathError{Op: "read", Path: r.path, Err: err}
		}
	}
}
