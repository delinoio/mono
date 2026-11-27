// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestBuiltOwnsandFIFODownload invokes the supplied production CLI binary
// against a disposable loopback Connect relay backed by controlled bytes.
func TestBuiltOwnsandFIFODownload(t *testing.T) {
	binary := os.Getenv("OWNSAND_FIFO_PROBE_BINARY")
	if !filepath.IsAbs(binary) {
		t.Fatal("OWNSAND_FIFO_PROBE_BINARY must name the built CLI by absolute path")
	}
	baseline := os.Getenv("OWNSAND_FIFO_EXPECT_STALL") == "1"
	r := newRunner(t)
	want := largeFIFOReadSource(r)
	t.Logf("Loopback relay payload: %d bytes, SHA-256 %x", len(want), sha256.Sum256(want))

	startRead := func(t *testing.T, destination, guestPath string) (*exec.Cmd, *bytes.Buffer, <-chan struct{}, *error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		t.Cleanup(cancel)
		cmd := exec.CommandContext(ctx, binary, "--no-color", "file", "read", "--to", destination, r.f.sb, guestPath)
		cmd.Env = []string{"TMPDIR=" + os.Getenv("TMPDIR")}
		for key, value := range r.env {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		var waitErr error
		done := make(chan struct{})
		go func() {
			waitErr = cmd.Wait()
			close(done)
		}()
		t.Cleanup(func() {
			_ = cmd.Process.Kill()
			<-done
		})
		return cmd, &stderr, done, &waitErr
	}

	t.Run("idle_reader_one_SIGINT", func(t *testing.T) {
		fifo := makeFIFO(t)
		fd, err := unix.Open(fifo, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer unix.Close(fd)
		cmd, stderr, done, waitErr := startRead(t, fifo, largeFIFOReadPath)

		fillCtx, stop := context.WithTimeout(t.Context(), 10*time.Second)
		err = waitForFullTestFIFO(fillCtx, fifo, fd)
		stop()
		if err != nil {
			t.Fatal(err)
		}
		t.Log("Confirmed the attached reader had data and the FIFO was no longer writable. The reader stayed idle.")
		sent := time.Now()
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			t.Fatal(err)
		}
		select {
		case <-done:
			if baseline {
				t.Fatalf("expected the original CLI to remain blocked, got %v, stderr %q", *waitErr, stderr.String())
			}
		case <-time.After(5 * time.Second):
			if !baseline {
				t.Fatal("the CLI remained blocked five seconds after one SIGINT")
			}
			t.Log("Original CLI remained blocked for five seconds after one SIGINT with the reader still idle.")
			return
		}
		var exitErr *exec.ExitError
		if !errors.As(*waitErr, &exitErr) || exitErr.ExitCode() != 1 || stderr.String() != "error: context canceled\n" {
			t.Fatalf("want exit 1 with context canceled, got %v, stderr %q", *waitErr, stderr.String())
		}
		t.Logf("One SIGINT produced exit 1 and %q in %s, before the reader drained.", strings.TrimSpace(stderr.String()), time.Since(sent).Round(time.Millisecond))

		drainCtx, stop := context.WithTimeout(t.Context(), 5*time.Second)
		defer stop()
		got, err := io.ReadAll(testFIFOReader{ctx: drainCtx, fd: fd})
		if err != nil {
			t.Fatalf("drain the accepted bytes through EOF: %v", err)
		}
		if info, err := os.Lstat(fifo); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
			t.Fatalf("destination is no longer a FIFO: %v, %v", info, err)
		}
		if len(got) == 0 || len(got) >= len(want) || !bytes.HasPrefix(want, got) {
			t.Fatalf("invalid prefix: %d of %d bytes", len(got), len(want))
		}
		t.Logf("FIFO stayed in place and retained an exact %d-byte prefix followed by EOF.", len(got))
	})
	if baseline {
		return
	}

	t.Run("draining_reader", func(t *testing.T) {
		fifo := makeFIFO(t)
		_, stderr, done, waitErr := startRead(t, fifo, largeFIFOReadPath)
		readCtx, stop := context.WithTimeout(t.Context(), 10*time.Second)
		defer stop()
		reader, err := openTestFIFOReader(readCtx, fifo)
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		fd := int(reader.Fd())
		if err := unix.SetNonblock(fd, true); err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(testFIFOReader{ctx: readCtx, fd: fd})
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("FIFO read did not return the exact contents: got %d of %d bytes, error %v", len(got), len(want), err)
		}
		<-done
		if *waitErr != nil {
			t.Fatalf("CLI failed: %v, stderr %q", *waitErr, stderr.String())
		}
		t.Logf("CLI exited 0 and delivered all %d bytes in order through EOF.", len(got))
	})

	t.Run("regular_file_replacement", func(t *testing.T) {
		dir := t.TempDir()
		destination := filepath.Join(dir, "out")
		prior := []byte("previous contents")
		if err := os.WriteFile(destination, prior, 0o640); err != nil {
			t.Fatal(err)
		}
		_, stderr, done, waitErr := startRead(t, destination, "/missing")
		<-done
		var exitErr *exec.ExitError
		if !errors.As(*waitErr, &exitErr) || exitErr.ExitCode() != 1 || !strings.Contains(stderr.String(), "not_found") {
			t.Fatalf("missing-source read: %v, stderr %q", *waitErr, stderr.String())
		}
		got, err := os.ReadFile(destination)
		if err != nil || !bytes.Equal(got, prior) {
			t.Fatalf("failed read changed the destination: %v", err)
		}
		_, stderr, done, waitErr = startRead(t, destination, largeFIFOReadPath)
		<-done
		if *waitErr != nil {
			t.Fatalf("CLI failed: %v, stderr %q", *waitErr, stderr.String())
		}
		got, err = os.ReadFile(destination)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("successful read did not replace the destination with all bytes: %v", err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 1 {
			t.Fatalf("download left staging files: %v, %v", entries, err)
		}
		t.Log("A missing source preserved the original file. A successful read replaced it with the exact payload and left no staging files.")
	})
}
