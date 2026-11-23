// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"runtime"
	"syscall"
	"testing"
)

// The errno rules run on every host: the errno values and the platform name
// are injected, so the Windows rule is covered on Linux and the Unix rule on
// Windows.
func TestIsAddressInUseErrno(t *testing.T) {
	cases := []struct {
		name  string
		errno syscall.Errno
		goos  string
		want  bool
	}{
		{"unix EADDRINUSE", syscall.EADDRINUSE, "linux", true},
		{"darwin EADDRINUSE", syscall.EADDRINUSE, "darwin", true},
		{"unix permission", syscall.EACCES, "linux", false},
		{"windows native WSAEADDRINUSE", 10048, "windows", true},
		{"windows WSAEACCES", 10013, "windows", false},
		{"windows access denied", 5, "windows", false},
		// A Unix errno number must not be taken for the Windows error.
		{"windows with a Unix number", 98, "windows", false},
	}
	for _, tc := range cases {
		if got := isAddressInUseErrno(tc.errno, tc.goos); got != tc.want {
			t.Errorf("%s: isAddressInUseErrno(%d, %q) = %v, want %v", tc.name, uintptr(tc.errno), tc.goos, got, tc.want)
		}
	}
}

// bindError builds the error shape of a failed net.Listen: an OpError that
// wraps an os.SyscallError that wraps the errno.
func bindError(errno syscall.Errno) error {
	return &net.OpError{Op: "listen", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}, Err: os.NewSyscallError("bind", errno)}
}

func TestClassifyBindErrorForWrappedErrno(t *testing.T) {
	cases := []struct {
		name string
		err  error
		goos string
		want startupFailureReason
	}{
		{"wrapped EADDRINUSE", bindError(syscall.EADDRINUSE), "linux", startupFailureReasonAddressInUse},
		{"wrapped Winsock 10048", bindError(10048), "windows", startupFailureReasonAddressInUse},
		{"doubly wrapped", fmt.Errorf("listen: %w", bindError(syscall.EADDRINUSE)), "linux", startupFailureReasonAddressInUse},
		{"permission denied", bindError(syscall.EACCES), "linux", ""},
		{"Winsock access denied", bindError(10013), "windows", ""},
		{"not an errno", errors.New("address already in use"), "linux", ""},
		{"Windows text without the errno", errors.New("Only one usage of each socket address (protocol/network address/port) is normally permitted."), "windows", ""},
	}
	for _, tc := range cases {
		got := classifyBindErrorFor(tc.err, tc.goos)
		if reason := startupFailureReasonOf(got); reason != tc.want {
			t.Errorf("%s: reason = %q, want %q", tc.name, reason, tc.want)
		}
		// The original error stays reachable and unchanged in text.
		if !errors.Is(got, tc.err) || got.Error() != tc.err.Error() {
			t.Errorf("%s: the classified error does not keep the original error", tc.name)
		}
	}
	if classifyBindErrorFor(nil, "linux") != nil {
		t.Error("classifyBindErrorFor(nil) must be nil")
	}
}

// A real bind collision on the running platform produces the native errno
// (EADDRINUSE on Unix, Winsock 10048 on Windows) that the classifier must
// recognize, for HTTP and for STUN.
func TestClassifyRealBindCollisions(t *testing.T) {
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	_, err = net.Listen("tcp", tcp.Addr().String())
	if err == nil {
		t.Fatal("a second TCP listener on a held address succeeded")
	}
	if got := startupFailureReasonOf(classifyBindError(err)); got != startupFailureReasonAddressInUse {
		t.Errorf("TCP collision on %s: reason = %q, want %q", runtime.GOOS, got, startupFailureReasonAddressInUse)
	}

	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	_, err = net.ListenPacket("udp", udp.LocalAddr().String())
	if err == nil {
		t.Fatal("a second UDP listener on a held address succeeded")
	}
	if got := startupFailureReasonOf(classifyBindError(err)); got != startupFailureReasonAddressInUse {
		t.Errorf("UDP collision on %s: reason = %q, want %q", runtime.GOOS, got, startupFailureReasonAddressInUse)
	}
}

func TestLogServerExitedCarriesTheReasonAndTheError(t *testing.T) {
	record := func(err error) map[string]any {
		t.Helper()
		var buf bytes.Buffer
		logServerExited(slog.New(slog.NewJSONHandler(&buf, nil)), err)
		var m map[string]any
		if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
			t.Fatalf("server_exited is not one JSON record: %v", err)
		}
		return m
	}

	inUse := bindError(syscall.EADDRINUSE)
	m := record(classifyBindErrorFor(inUse, "linux"))
	if m["msg"] != "server_exited" || m[startupFailureReasonKey] != "address_in_use" || m["error"] != inUse.Error() {
		t.Errorf("server_exited = %v, want msg, startup_failure_reason=address_in_use and the original error", m)
	}

	other := errors.New("config: DATABASE_URL is required")
	m = record(other)
	if _, ok := m[startupFailureReasonKey]; ok {
		t.Errorf("an unclassified failure carries startup_failure_reason: %v", m)
	}
	if m["error"] != other.Error() {
		t.Errorf("error = %v, want %q", m["error"], other.Error())
	}
}
