// SPDX-License-Identifier: FSL-1.1-ALv2

package vzrt

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
)

// TestDrainConsoleKeepsTheCapAndNeverBlocksTheProducer writes 1 MiB to the
// pipe in 4 KiB chunks: every write completes (the pipe never fills), the
// drain returns after the write end closes, and the shared writer keeps the
// first 64 bytes only.
func TestDrainConsoleKeepsTheCapAndNeverBlocksTheProducer(t *testing.T) {
	const limit = 64
	path := filepath.Join(t.TempDir(), "console.log")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	w, err := vmruntime.OpenCappedLog(path, limit, logger, "sbx", vmruntime.ConsoleLogKindConsole)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	r, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	drained := make(chan struct{})
	go func() {
		drainConsole(r, w)
		close(drained)
	}()

	want := make([]byte, 0, 1<<20)
	written := make(chan error, 1)
	go func() {
		defer pw.Close()
		chunk := make([]byte, 4096)
		for i := 0; i < (1<<20)/len(chunk); i++ {
			for j := range chunk {
				chunk[j] = byte('a' + (i+j)%26)
			}
			want = append(want, chunk...)
			if _, err := pw.Write(chunk); err != nil {
				written <- err
				return
			}
		}
		written <- nil
	}()
	select {
	case err := <-written:
		if err != nil {
			t.Fatalf("producer write failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the producer blocked: the pipe was not drained")
	}
	select {
	case <-drained:
	case <-time.After(10 * time.Second):
		t.Fatal("drainConsole did not return after the write end closed")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want[:limit]) {
		t.Fatalf("console.log = %d bytes, want the first %d bytes of the output", len(got), limit)
	}
	// drainConsole closed the read end.
	if _, err := r.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("read after drainConsole = %v, want os.ErrClosed", err)
	}
}

// TestDrainConsoleEndsOnReadError checks that a closed read end (the abort
// path of a failed VM configuration) ends the drain.
func TestDrainConsoleEndsOnReadError(t *testing.T) {
	r, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pw.Close()
	var sink bytes.Buffer
	drained := make(chan struct{})
	go func() {
		drainConsole(r, &sink)
		close(drained)
	}()
	r.Close()
	select {
	case <-drained:
	case <-time.After(10 * time.Second):
		t.Fatal("drainConsole did not return after its read end closed")
	}
}
