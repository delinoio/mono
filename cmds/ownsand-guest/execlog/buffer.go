// SPDX-License-Identifier: FSL-1.1-ALv2

// Package execlog keeps the recent output and the result of one exec and
// turns them into the ordered ExecEvent sequence of the protos contract. The
// guest helper uses it for the processes it runs, and the device agent uses
// it for the output it keeps for AttachExec. Output is sandbox content:
// nothing in this package logs it.
package execlog

// Buffer keeps the last Capacity bytes of one output stream (stdout or
// stderr) together with their absolute byte offsets in the stream. It is not
// safe for concurrent use; Log serializes access.
type Buffer struct {
	capacity int
	data     []byte // data[head:] holds the bytes [start, end)
	head     int
	start    int64
	end      int64
}

// NewBuffer returns an empty buffer that keeps at most capacity bytes.
func NewBuffer(capacity int) *Buffer {
	if capacity <= 0 {
		panic("execlog: buffer capacity must be positive")
	}
	return &Buffer{capacity: capacity}
}

// Write appends p to the stream and drops the oldest bytes beyond the
// capacity.
func (b *Buffer) Write(p []byte) {
	b.end += int64(len(p))
	if len(p) >= b.capacity {
		b.data = append(b.data[:0], p[len(p)-b.capacity:]...)
		b.head = 0
		b.start = b.end - int64(b.capacity)
		return
	}
	b.data = append(b.data, p...)
	if kept := len(b.data) - b.head; kept > b.capacity {
		drop := kept - b.capacity
		b.head += drop
		b.start += int64(drop)
	}
	// Compact once the dropped prefix is as large as the capacity, so the
	// slice never grows beyond twice the capacity.
	if b.head >= b.capacity {
		n := copy(b.data, b.data[b.head:])
		b.data = b.data[:n]
		b.head = 0
	}
}

// Start returns the offset of the oldest kept byte.
func (b *Buffer) Start() int64 { return b.start }

// End returns the total number of bytes written to the stream.
func (b *Buffer) End() int64 { return b.end }

// Read returns a copy of at most max kept bytes from offset off. It returns
// nil when off is at or beyond End. The caller must not ask for an offset
// before Start.
func (b *Buffer) Read(off int64, max int) []byte {
	if off < b.start {
		off = b.start
	}
	if off >= b.end || max <= 0 {
		return nil
	}
	i := b.head + int(off-b.start)
	n := min(max, len(b.data)-i)
	out := make([]byte, n)
	copy(out, b.data[i:i+n])
	return out
}
