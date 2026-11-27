// SPDX-License-Identifier: FSL-1.1-ALv2

package guestlisten

import (
	"errors"
	"fmt"
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

// The kernel returns ENODEV when it cannot select a transport and
// ENETUNREACH when the selected transport cannot reach VMADDR_CID_LOCAL.
func rawVsockLoopbackUnavailable(err error) bool {
	return errors.Is(err, unix.ENODEV) || errors.Is(err, unix.ENETUNREACH)
}

func TestRawVsockLoopbackUnavailable(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "connected"},
		{name: "missing transport", err: unix.ENODEV, want: true},
		{name: "local CID unreachable", err: unix.ENETUNREACH, want: true},
		{name: "connection refused", err: unix.ECONNREFUSED},
		{name: "connection timed out", err: unix.ETIMEDOUT},
		{name: "invalid address", err: unix.EINVAL},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := rawVsockLoopbackUnavailable(tt.err); got != tt.want {
				t.Errorf("rawVsockLoopbackUnavailable(%v) = %t, want %t", tt.err, got, tt.want)
			}
		})
	}
}

func requireVsockLoopback(t *testing.T) {
	t.Helper()
	requireVsockSocket(t)

	listener, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open raw vsock probe listener: %v", err)
	}
	defer unix.Close(listener)
	if err := unix.Bind(listener, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: unix.VMADDR_PORT_ANY}); err != nil {
		t.Fatalf("bind raw vsock probe listener: %v", err)
	}
	if err := unix.Listen(listener, 1); err != nil {
		t.Fatalf("listen on raw vsock probe socket: %v", err)
	}
	address, err := unix.Getsockname(listener)
	if err != nil {
		t.Fatalf("get raw vsock probe listener address: %v", err)
	}
	bound, ok := address.(*unix.SockaddrVM)
	if !ok || bound.Port == unix.VMADDR_PORT_ANY || bound.Port == 0 {
		t.Fatalf("raw vsock probe listener has invalid address: %v", address)
	}

	client, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open raw vsock probe client: %v", err)
	}
	defer unix.Close(client)
	timeout := unix.NsecToTimeval((2 * time.Second).Nanoseconds())
	if err := unix.SetsockoptTimeval(client, unix.AF_VSOCK, unix.SO_VM_SOCKETS_CONNECT_TIMEOUT, &timeout); err != nil {
		t.Fatalf("set raw vsock probe connect timeout: %v", err)
	}
	if err := unix.Connect(client, &unix.SockaddrVM{CID: unix.VMADDR_CID_LOCAL, Port: bound.Port}); err != nil {
		if rawVsockLoopbackUnavailable(err) {
			t.Skipf("raw vsock probe could not connect to VMADDR_CID_LOCAL, local transport unavailable: %v", err)
		}
		t.Fatalf("raw vsock probe connect to VMADDR_CID_LOCAL: %v", err)
	}
}

// TestVsockLoopback needs the vsock loopback transport (CID 1) of the host
// kernel; it skips when the host has none. A stream carries no message
// boundaries, so the payload is read to its full length and one case delivers
// it in two writes.
func TestVsockLoopback(t *testing.T) {
	requireVsockLoopback(t)

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
				t.Fatalf("listenVsock: %v", err)
			}
			defer l.Close()
			address := l.Addr()
			addr, ok := address.(*VsockAddr)
			if !ok || addr == nil {
				t.Fatalf("listenVsock returned an unexpected address: %v", address)
			}
			fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
			if err != nil {
				t.Fatalf("open vsock loopback client socket: %v", err)
			}
			defer unix.Close(fd)
			if err := unix.Connect(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_LOCAL, Port: addr.Port}); err != nil {
				t.Fatalf("connect vsock loopback client to VMADDR_CID_LOCAL port %d: %v", addr.Port, err)
			}

			done := make(chan vsockReceived, 1)
			go func() {
				c, err := l.Accept()
				if err != nil {
					done <- vsockReceived{err: fmt.Errorf("accept vsock loopback client: %w", err)}
					return
				}
				defer c.Close()
				if err := c.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
					done <- vsockReceived{err: fmt.Errorf("set accepted vsock connection deadline: %w", err)}
					return
				}
				buf := make([]byte, len(want))
				n, err := io.ReadFull(c, buf)
				if err != nil {
					err = fmt.Errorf("read %d of %d bytes (%q): %w", n, len(want), buf[:n], err)
				}
				done <- vsockReceived{data: buf[:n], err: err}
			}()
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
					t.Fatal(got.err)
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
