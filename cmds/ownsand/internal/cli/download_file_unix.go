// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin

package cli

import (
	"context"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func downloadFileWriter(ctx context.Context, f *os.File) (io.Writer, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeNamedPipe == 0 {
		return f, nil
	}

	// The FIFO is open, so switching to nonblocking mode preserves the
	// initial wait for a reader. Use direct writes because Go does not
	// support FIFO deadlines on macOS.
	fd := int(f.Fd())
	if err := unix.SetNonblock(fd, true); err != nil {
		return nil, &os.PathError{Op: "set nonblocking", Path: f.Name(), Err: err}
	}
	return &fifoDownloadWriter{ctx: ctx, fd: fd, path: f.Name()}, nil
}

type fifoDownloadWriter struct {
	ctx  context.Context
	fd   int
	path string
}

func (w *fifoDownloadWriter) Write(p []byte) (int, error) {
	written := 0
	for written < len(p) {
		if err := w.ctx.Err(); err != nil {
			return written, err
		}
		n, err := unix.Write(w.fd, p[written:])
		if n > 0 {
			written += n
		}
		switch err {
		case nil:
			if n == 0 {
				return written, io.ErrShortWrite
			}
		case unix.EINTR:
			continue
		case unix.EAGAIN:
			// Bound the wait so cancellation works even if the reader stays idle.
			const cancelPollMilliseconds = 50
			fds := [1]unix.PollFd{{Fd: int32(w.fd), Events: unix.POLLOUT}}
			if _, err := unix.Poll(fds[:], cancelPollMilliseconds); err != nil && err != unix.EINTR {
				return written, &os.PathError{Op: "poll", Path: w.path, Err: err}
			}
		default:
			return written, &os.PathError{Op: "write", Path: w.path, Err: err}
		}
	}
	return written, nil
}
