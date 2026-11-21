// SPDX-License-Identifier: Apache-2.0

package fcrt

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// consoleLogLimit is the per-sandbox cap of firecracker.log in bytes: the
// first 1 MiB of the Firecracker output (stdout and stderr, which include
// the guest serial console) is kept and the rest is discarded.
const consoleLogLimit int64 = 1 << 20

// cappedLog is the one writer behind both the stdout and the stderr of a
// firecracker process. It keeps the first limit bytes that a VM directory's
// log receives over all launches of that directory (a memory-state restore
// that falls back to a cold boot starts a second process) and consumes and
// discards everything after that, so the child never blocks on a full pipe
// and never sees a write error. It never logs the console content.
type cappedLog struct {
	logger    *slog.Logger
	sandboxID string

	mu      sync.Mutex
	f       *os.File
	limit   int64
	written int64 // bytes in the file, including bytes of earlier launches
	capped  bool  // the cap event was logged
	failed  bool  // a write failure was logged; the file is not written again
}

// openCappedLog opens (creating it with mode 0600 when needed) the log file
// at path for appending. A file that is already larger than limit, for
// example written by an agent version without a cap, is truncated to limit.
func openCappedLog(path string, limit int64, logger *slog.Logger, sandboxID string) (*cappedLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*cappedLog, error) {
		f.Close()
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		return fail(fmt.Errorf("fcrt: stat %s: %w", filepath.Base(path), err))
	}
	size := st.Size()
	if size > limit {
		if err := f.Truncate(limit); err != nil {
			return fail(fmt.Errorf("fcrt: trim %s: %w", filepath.Base(path), err))
		}
		logger.Warn("oversized firecracker log trimmed to the cap", "sandbox_id", sandboxID, "size_bytes", size, "limit_bytes", limit)
		size = limit
	}
	return &cappedLog{logger: logger, sandboxID: sandboxID, f: f, limit: limit, written: size}, nil
}

// Write keeps the part of p that fits below the cap and drops the rest. It
// always reports len(p) consumed bytes and no error.
func (c *cappedLog) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	keep := c.limit - c.written
	if keep > int64(len(p)) {
		keep = int64(len(p))
	}
	if keep > 0 && !c.failed {
		n, err := c.f.Write(p[:keep])
		c.written += int64(n)
		if err != nil {
			c.failed = true
			c.logger.Warn("firecracker log write failed; discarding further output", "sandbox_id", c.sandboxID, "error", err)
		}
	}
	if int64(len(p)) > keep && !c.capped {
		c.capped = true
		c.logger.Warn("firecracker log reached the cap; discarding further output", "sandbox_id", c.sandboxID, "limit_bytes", c.limit)
	}
	return len(p), nil
}

// Close closes the file. Call it after the process was waited for, so the
// copy of its output ended.
func (c *cappedLog) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.f.Close()
}
