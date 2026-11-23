// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"io"
)

// ctxReader is an io.Reader whose Read returns when ctx is done, even while
// the underlying reader is blocked. A blocking read of a terminal or a pipe
// cannot observe a context, so each Read runs the underlying Read in a
// goroutine and waits for the result or for ctx.
//
// When ctx ends a Read first, the goroutine stays blocked in the underlying
// Read until that Read returns; nothing waits for it, so it cannot delay the
// end of the process. The goroutine reads into the reader's own buffer and
// the caller's slice is never written after Read has returned, so there is no
// race on the caller's buffer. After ctx is done, every Read returns
// ctx.Err() and starts no new read, so at most one abandoned read exists.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
	buf []byte
}

type readResult struct {
	n   int
	err error
}

func newCtxReader(ctx context.Context, r io.Reader) *ctxReader {
	return &ctxReader{ctx: ctx, r: r}
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if len(c.buf) < len(p) {
		c.buf = make([]byte, len(p))
	}
	buf := c.buf[:len(p)]
	// The channel has room for the result, so an abandoned goroutine never
	// blocks on the send.
	done := make(chan readResult, 1)
	go func() {
		n, err := c.r.Read(buf)
		done <- readResult{n, err}
	}()
	select {
	case res := <-done:
		copy(p, buf[:res.n])
		return res.n, res.err
	case <-c.ctx.Done():
		return 0, c.ctx.Err()
	}
}
