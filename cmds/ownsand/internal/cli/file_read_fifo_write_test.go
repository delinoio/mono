// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin

package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const largeFIFOReadPath = "/tmp/large-fifo-source"

func largeFIFOReadSource(r *runner) []byte {
	data := bytes.Repeat([]byte("OwnSand FIFO transfer\x00\xff\n"), 65_536)
	r.f.mu.Lock()
	r.f.files[largeFIFOReadPath] = data
	r.f.mu.Unlock()
	return data
}

func waitForFullTestFIFO(ctx context.Context, fifo string, readerFD int) error {
	// The probe writer never writes. Readable data must come from the CLI.
	probeFD, err := unix.Open(fifo, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(probeFD)
	fds := [2]unix.PollFd{
		{Fd: int32(readerFD), Events: unix.POLLIN},
		{Fd: int32(probeFD), Events: unix.POLLOUT},
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, err := unix.Poll(fds[:], 0)
		switch err {
		case nil:
			if fds[0].Revents&unix.POLLIN != 0 && fds[1].Revents&unix.POLLOUT == 0 {
				return nil
			}
		case unix.EINTR:
		default:
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for full FIFO: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestFileReadFIFOWithFullPipeEndsOnCancel(t *testing.T) {
	r := newRunner(t)
	want := largeFIFOReadSource(r)
	fifo := makeFIFO(t)
	fd, err := unix.Open(fifo, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan result, 1)
	go func() {
		done <- r.runCtx(ctx, "--no-color", "file", "read", "--to", fifo, r.f.sb, largeFIFOReadPath)
	}()

	fillCtx, stop := context.WithTimeout(t.Context(), 5*time.Second)
	err = waitForFullTestFIFO(fillCtx, fifo, fd)
	stop()
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case res := <-done:
		if res.code != ExitError || res.stderr != "error: context canceled\n" {
			t.Fatalf("want exit %d and a cancellation error, got %+v", ExitError, res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the command did not end after cancellation while the FIFO reader stayed idle")
	}

	drainCtx, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	got, err := io.ReadAll(testFIFOReader{ctx: drainCtx, fd: fd})
	if err != nil {
		t.Fatalf("drain FIFO through EOF: %v", err)
	}
	if len(got) == 0 || len(got) >= len(want) || !bytes.HasPrefix(want, got) {
		t.Fatalf("FIFO did not retain an exact partial prefix: got %d of %d bytes", len(got), len(want))
	}
	if info, err := os.Lstat(fifo); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("the destination is no longer a FIFO: %v, %v", info, err)
	}
}

func TestFileReadFIFOLargeDownloadWithDrainingReader(t *testing.T) {
	r := newRunner(t)
	want := largeFIFOReadSource(r)
	fifo := makeFIFO(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan result, 1)
	go func() {
		done <- r.runCtx(ctx, "file", "read", "--to", fifo, r.f.sb, largeFIFOReadPath)
	}()

	reader, err := openTestFIFOReader(ctx, fifo)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	fd := int(reader.Fd())
	if err := unix.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(testFIFOReader{ctx: ctx, fd: fd})
	if err != nil {
		t.Fatalf("drain FIFO through EOF: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("FIFO contents differ: got %d bytes, want %d", len(got), len(want))
	}
	select {
	case res := <-done:
		if res.code != 0 {
			t.Fatalf("read --to FIFO: %+v", res)
		}
	case <-ctx.Done():
		t.Fatalf("the command did not end after the FIFO reached EOF: %v", ctx.Err())
	}
}
