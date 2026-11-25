// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func openTestFIFOWriter(t *testing.T, fifo string) *os.File {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		f, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			return f
		}
		if !errors.Is(err, syscall.ENXIO) || time.Now().After(deadline) {
			t.Fatalf("open FIFO writer: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestFileWriteFIFOWithoutWriterEndsOnCancel(t *testing.T) {
	r := newRunner(t)
	fifo := makeFIFO(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan result, 1)
	go func() {
		done <- r.runCtx(ctx, "file", "write", r.f.sb, "/tmp/x", "--from", fifo)
	}()
	select {
	case res := <-done:
		t.Fatalf("the command ended before a writer opened the FIFO: %+v", res)
	case <-time.After(300 * time.Millisecond):
	}
	cancel()
	select {
	case res := <-done:
		if res.code != ExitError || !strings.Contains(res.stderr, context.Canceled.Error()) {
			t.Errorf("want exit %d and a cancellation error, got %+v", ExitError, res)
		}
	case <-time.After(5 * time.Second):
		// Release the blocked open even if cancellation failed.
		writer := openTestFIFOWriter(t, fifo)
		_ = writer.Close()
		t.Fatal("the command did not end after cancellation")
	}

	// A late writer releases the blocked open. Once the abandoned reader
	// closes, a nonblocking writer open must fail with ENXIO.
	writer := openTestFIFOWriter(t, fifo)
	defer writer.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		probe, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if errors.Is(err, syscall.ENXIO) {
			break
		}
		if err != nil {
			t.Fatalf("probe FIFO for readers: %v", err)
		}
		_ = probe.Close()
		if time.Now().After(deadline) {
			t.Fatal("the abandoned open kept the FIFO read end open")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if calls := r.f.callNames(); len(calls) != 0 {
		t.Fatalf("API received calls during a canceled FIFO open: %v", calls)
	}
}

func TestFileWriteFIFOWithStalledWriterEndsOnCancel(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
	}{
		{name: "idle"},
		{name: "partial chunk", data: "partial first chunk"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRunner(t)
			fifo := makeFIFO(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan result, 1)
			go func() {
				done <- r.runCtx(ctx, "--no-color", "file", "write", r.f.sb, "/tmp/x", "--from", fifo)
			}()
			writer := openTestFIFOWriter(t, fifo)
			defer writer.Close()
			if tc.data != "" {
				if _, err := writer.WriteString(tc.data); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case res := <-done:
				t.Fatalf("the command ended while the writer was still open: %+v", res)
			case <-time.After(100 * time.Millisecond):
			}
			cancel()
			select {
			case res := <-done:
				if res.code != ExitInterrupted || res.stderr != "error: interrupted; the write was not finished\n" {
					t.Fatalf("want exit %d and the interruption message, got %+v", ExitInterrupted, res)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the command did not end while the FIFO writer stayed open")
			}
			if calls := r.f.callNames(); len(calls) != 0 {
				t.Fatalf("API received calls before the first chunk: %v", calls)
			}
			probe, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0)
			if probe != nil {
				_ = probe.Close()
			}
			if !errors.Is(err, syscall.ENXIO) {
				t.Fatalf("want the FIFO reader closed after cancellation, got %v", err)
			}
		})
	}
}

func TestFileWriteFIFOWithWriterSendsBytes(t *testing.T) {
	r := newRunner(t)
	fifo := makeFIFO(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan result, 1)
	go func() {
		done <- r.runCtx(ctx, "file", "write", r.f.sb, "/tmp/x", "--from", fifo)
	}()
	writer := openTestFIFOWriter(t, fifo)
	want := []byte("FIFO contents\x00\xff")
	_, writeErr := writer.Write(want)
	closeErr := writer.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		t.Fatal(err)
	}
	select {
	case res := <-done:
		if res.code != ExitOK || res.stderr != fmt.Sprintf("Wrote %d bytes.\n", len(want)) {
			t.Fatalf("write from FIFO: %+v", res)
		}
	case <-ctx.Done():
		t.Fatal("the command did not finish the FIFO upload")
	}
	r.f.mu.Lock()
	got := r.f.files["/tmp/x"]
	r.f.mu.Unlock()
	if !bytes.Equal(got, want) {
		t.Fatalf("uploaded bytes = %q, want %q", got, want)
	}
}
