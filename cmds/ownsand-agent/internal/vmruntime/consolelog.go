// SPDX-License-Identifier: FSL-1.1-ALv2

package vmruntime

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// ConsoleLogLimit is the per-sandbox cap of a console log file in bytes: the
// first 1 MiB of the guest console output is kept and the rest is discarded.
// The guest controls this output, so a file without a cap would let a guest
// fill the host disk for the life of its sandbox.
const ConsoleLogLimit int64 = 1 << 20

// ConsoleLogKind names the file that a CappedLog writes. It selects the
// wording of the structured log events, so each runtime keeps its own
// stable messages.
type ConsoleLogKind int

const (
	// ConsoleLogKindUnspecified is the zero value; OpenCappedLog rejects it.
	ConsoleLogKindUnspecified ConsoleLogKind = iota
	// ConsoleLogKindFirecracker is firecracker.log: the stdout and stderr of
	// a firecracker process, which include the guest serial console.
	ConsoleLogKindFirecracker
	// ConsoleLogKindConsole is console.log: the COM1 output of a Hyper-V
	// guest, or the virtio console of a Virtualization.framework Linux
	// guest.
	ConsoleLogKindConsole
)

// subject is the noun that the log events of the kind use.
func (k ConsoleLogKind) subject() string {
	switch k {
	case ConsoleLogKindFirecracker:
		return "firecracker log"
	default:
		return "console log"
	}
}

// valid reports whether k is a defined, specified kind.
func (k ConsoleLogKind) valid() bool {
	return k == ConsoleLogKindFirecracker || k == ConsoleLogKindConsole
}

// CappedLog is a writer that keeps the first limit bytes that a log file
// receives over all of its openings (a memory-state restore that falls back
// to a cold boot starts a second Firecracker process or a second VM, and the
// Hyper-V runtime re-dials the console pipe) and consumes and discards everything
// after that, so the producer never blocks on a full pipe and never sees a
// write error. It is safe for concurrent writers. It never logs the console
// content.
type CappedLog struct {
	logger    *slog.Logger
	sandboxID string
	subject   string

	mu      sync.Mutex
	f       *os.File
	limit   int64
	written int64 // bytes in the file, including bytes of earlier openings
	capped  bool  // the cap event was logged
	failed  bool  // a write failure was logged; the file is not written again
	closed  bool
}

// OpenCappedLog opens (creating it with mode 0600 when needed) the log file
// at path for appending. A file that is already larger than limit, for
// example written by an agent version without a cap, is truncated to limit.
// kind selects the wording of the log events.
func OpenCappedLog(path string, limit int64, logger *slog.Logger, sandboxID string, kind ConsoleLogKind) (*CappedLog, error) {
	if !kind.valid() {
		return nil, fmt.Errorf("vmruntime: unsupported console log kind %d", int(kind))
	}
	subject := kind.subject()
	// Trim an oversized file before opening it for append. On Windows, Go opens
	// O_APPEND handles with append-only access, which cannot truncate the
	// file, so the trim must not go through the handle that is kept for
	// writing.
	trimmedFrom := int64(-1)
	if st, err := os.Stat(path); err == nil && st.Size() > limit {
		if err := os.Truncate(path, limit); err != nil {
			return nil, fmt.Errorf("vmruntime: trim %s: %w", filepath.Base(path), err)
		}
		trimmedFrom = st.Size()
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("vmruntime: stat %s: %w", filepath.Base(path), err)
	}
	size := st.Size()
	if trimmedFrom >= 0 {
		logger.Warn("oversized "+subject+" trimmed to the cap", "sandbox_id", sandboxID, "size_bytes", trimmedFrom, "limit_bytes", limit)
	}
	return &CappedLog{logger: logger, sandboxID: sandboxID, subject: subject, f: f, limit: limit, written: size}, nil
}

// Write keeps the part of p that fits below the cap and drops the rest. It
// always reports len(p) consumed bytes and no error, also after Close.
func (c *CappedLog) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	keep := c.limit - c.written
	if keep > int64(len(p)) {
		keep = int64(len(p))
	}
	if keep > 0 && !c.failed && !c.closed {
		n, err := c.f.Write(p[:keep])
		c.written += int64(n)
		if err != nil {
			c.failed = true
			c.logger.Warn(c.subject+" write failed; discarding further output", "sandbox_id", c.sandboxID, "error", err)
		}
	}
	if !c.failed && !c.closed && int64(len(p)) > keep && !c.capped {
		c.capped = true
		c.logger.Warn(c.subject+" reached the cap; discarding further output", "sandbox_id", c.sandboxID, "limit_bytes", c.limit)
	}
	return len(p), nil
}

// Close closes the file. Call it after the producer ended, so the copy of
// its output ended. A later Write still consumes its bytes and discards
// them.
func (c *CappedLog) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.f.Close()
}
