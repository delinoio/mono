// SPDX-License-Identifier: FSL-1.1-ALv2

package guestlisten

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// vsockReceived is what the accepting side of a loopback test got.
type vsockReceived struct {
	data []byte
	err  error
}

// writeAll writes all of p to the raw socket. A stream socket may accept fewer
// bytes than requested, and a signal may interrupt the call, so it loops over
// partial counts and retries EINTR.
func writeAll(fd int, p []byte) error {
	for len(p) > 0 {
		n, err := unix.Write(fd, p)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}

// TestVsockLoopback needs the vsock loopback transport (CID 1) of the host
// kernel; it skips when the host has none. A stream carries no message
// boundaries, so the payload is read to its full length and one case delivers
// it in two writes.
func TestVsockLoopback(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string
	}{
		{name: "single write", chunks: []string{"hello"}},
		{name: "split writes", chunks: []string{"hel", "lo"}},
	}
	basePort := 40000 + uint32(os.Getpid()%10000)
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := strings.Join(tc.chunks, "")
			l, err := listenVsock(basePort + uint32(i))
			if err != nil {
				t.Skipf("vsock unavailable: %v", err)
			}
			defer l.Close()
			port := l.Addr().(*VsockAddr).Port
			done := make(chan vsockReceived, 1)
			go func() {
				c, err := l.Accept()
				if err != nil {
					done <- vsockReceived{err: err}
					return
				}
				defer c.Close()
				c.SetDeadline(time.Now().Add(5 * time.Second))
				buf := make([]byte, len(want))
				n, err := io.ReadFull(c, buf)
				done <- vsockReceived{data: buf[:n], err: err}
			}()
			fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
			if err != nil {
				t.Skipf("vsock socket: %v", err)
			}
			defer unix.Close(fd)
			if err := unix.Connect(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_LOCAL, Port: port}); err != nil {
				t.Skipf("vsock loopback connect: %v", err)
			}
			for j, chunk := range tc.chunks {
				if j > 0 {
					// Let the first chunk reach the reader on its own.
					time.Sleep(10 * time.Millisecond)
				}
				if err := writeAll(fd, []byte(chunk)); err != nil {
					t.Fatalf("write %q: %v", chunk, err)
				}
			}
			select {
			case got := <-done:
				if got.err != nil {
					t.Fatalf("read %d of %d bytes (%q): %v", len(got.data), len(want), got.data, got.err)
				}
				if string(got.data) != want {
					t.Fatalf("got %q, want %q", got.data, want)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("timeout waiting for the accepted connection to read the payload")
			}
		})
	}
}
