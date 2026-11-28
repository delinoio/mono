// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

const listenerProbeTimeout = time.Second

// Winsock reports WSAECONNREFUSED. On Windows syscall.ECONNREFUSED is a
// synthetic error code and does not match the error returned by connect.
const winsockConnRefused syscall.Errno = 10061

func tcpRefusalCode() syscall.Errno {
	if runtime.GOOS == "windows" {
		return winsockConnRefused
	}
	return syscall.ECONNREFUSED
}

func probeTCPRefusal(addr string, dial func(network, addr string) (net.Conn, error)) (bool, error) {
	conn, err := dial("tcp", addr)
	switch {
	case err == nil:
		_ = conn.Close()
		return false, nil
	case errors.Is(err, tcpRefusalCode()):
		return true, nil
	default:
		return false, fmt.Errorf("dial public listener at %s: %w", addr, err)
	}
}

func TestProbeTCPRefusalErrors(t *testing.T) {
	const addr = "127.0.0.1:12345"
	for _, tc := range []struct {
		name    string
		cause   error
		refused bool
	}{
		{name: "refused", cause: os.NewSyscallError("connect", tcpRefusalCode()), refused: true},
		{name: "timeout", cause: context.DeadlineExceeded},
		{name: "socket creation", cause: os.NewSyscallError("socket", syscall.EMFILE)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dialErr := &net.OpError{Op: "dial", Net: "tcp", Err: tc.cause}
			refused, err := probeTCPRefusal(addr, func(_, _ string) (net.Conn, error) {
				return nil, dialErr
			})
			if refused != tc.refused {
				t.Errorf("refused = %t, want %t", refused, tc.refused)
			}
			if tc.refused {
				if err != nil {
					t.Errorf("dial diagnostic = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, dialErr) || !strings.Contains(err.Error(), addr) {
				t.Errorf("dial diagnostic = %v, want the original dial error and target %s", err, addr)
			}
		})
	}
}

func TestProbeTCPRefusalLoopback(t *testing.T) {
	listener := newTestListener(t)
	addr := listener.Addr().String()
	dialer := net.Dialer{Timeout: listenerProbeTimeout}

	if refused, err := probeTCPRefusal(addr, dialer.Dial); refused || err != nil {
		t.Fatalf("open listener at %s: refused = %t, error = %v", addr, refused, err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener at %s: %v", addr, err)
	}
	if refused, err := probeTCPRefusal(addr, dialer.Dial); !refused || err != nil {
		t.Fatalf("closed listener at %s: refused = %t, error = %v", addr, refused, err)
	}
}
