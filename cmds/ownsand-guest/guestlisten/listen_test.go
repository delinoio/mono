// SPDX-License-Identifier: FSL-1.1-ALv2

package guestlisten

import (
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		spec string
		want Spec
		ok   bool
	}{
		{"vsock://:1024", Spec{Scheme: SchemeVsock, Port: 1024}, true},
		{"vsock://52", Spec{Scheme: SchemeVsock, Port: 52}, true},
		{"vsock://:0", Spec{}, false},
		{"vsock://:x", Spec{}, false},
		{"unix:///run/g.sock", Spec{Scheme: SchemeUnix, Address: "/run/g.sock"}, true},
		{"tcp://127.0.0.1:0", Spec{Scheme: SchemeTCP, Address: "127.0.0.1:0"}, true},
		{"udp://x", Spec{}, false},
		{"plain", Spec{}, false},
	}
	for _, c := range cases {
		got, err := Parse(c.spec)
		if (err == nil) != c.ok || (c.ok && got != c.want) {
			t.Errorf("Parse(%q) = %+v, %v", c.spec, got, err)
		}
	}
	if DefaultSpec != "vsock://:1024" {
		t.Fatalf("default spec %q", DefaultSpec)
	}
}

func TestListenUnixRemovesStaleSocket(t *testing.T) {
	// A short relative path: a unix socket path under a deep TMPDIR can
	// exceed the 108-byte limit.
	t.Chdir(t.TempDir())
	path := "g.sock"
	for range 2 {
		l, err := Listen("unix://" + path)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			c, err := l.Accept()
			if err == nil {
				c.Write([]byte("x"))
				c.Close()
			}
		}()
		c, err := net.Dial("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 1)
		if _, err := c.Read(buf); err != nil || buf[0] != 'x' {
			t.Fatalf("read %q %v", buf, err)
		}
		c.Close()
		// Leave the socket file behind like a crashed helper would.
		l.(*net.UnixListener).SetUnlinkOnClose(false)
		l.Close()
	}
}

func TestListenUnixKeepsNonSocketEntries(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("keep", []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir("dir", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("keep", "link"); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	for _, name := range []string{"keep", "dir", "link"} {
		_, err := Listen("unix://" + name)
		if !errors.Is(err, ErrAddressOccupied) {
			t.Fatalf("%s: err = %v", name, err)
		}
		if strings.Contains(err.Error(), name) {
			t.Fatalf("%s: error contains the path: %v", name, err)
		}
		if _, statErr := os.Lstat(name); statErr != nil {
			t.Fatalf("%s was removed: %v", name, statErr)
		}
	}
	if got, err := os.ReadFile("keep"); err != nil || string(got) != "data" {
		t.Fatalf("keep content %q %v", got, err)
	}
}

func TestListenUnixAbstractAddress(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("abstract unix addresses exist on Linux only")
	}
	t.Chdir(t.TempDir())
	name := fmt.Sprintf("ownsand-guest-test-%d", os.Getpid())
	// A regular file with the abstract name must stay untouched.
	if err := os.WriteFile("@"+name, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := Listen("unix://@" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if got, err := os.ReadFile("@" + name); err != nil || string(got) != "data" {
		t.Fatalf("file content %q %v", got, err)
	}
}

func TestHvsockNotImplemented(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows guests implement Hyper-V sockets; see hvsock_windows_test.go")
	}
	if _, err := Listen("hvsock://00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("hvsock: %v", err)
	}
}

// serveOnce accepts connections on l until it closes and answers each with
// one byte.
func serveOnce(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		c.Write([]byte("x"))
		c.Close()
	}
}

func requireAnswers(t *testing.T, path string) {
	t.Helper()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial the live listener: %v", err)
	}
	defer c.Close()
	buf := make([]byte, 1)
	if _, err := c.Read(buf); err != nil || buf[0] != 'x' {
		t.Fatalf("live listener answer %q %v", buf, err)
	}
}

func TestListenUnixPreservesLiveListener(t *testing.T) {
	t.Chdir(t.TempDir())
	path := "g.sock"
	first, err := Listen("unix://" + path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	go serveOnce(first)
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}

	second, err := Listen("unix://" + path)
	if err == nil {
		second.Close()
		t.Fatal("a second listener took the path of a live listener")
	}
	if !errors.Is(err, ErrAddressInUse) {
		t.Fatalf("err = %v, want ErrAddressInUse", err)
	}
	if strings.Contains(err.Error(), path) {
		t.Fatalf("error contains the path: %v", err)
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("socket file was replaced: %v", err)
	}
	requireAnswers(t, path)
}

func TestListenUnixRecoversAfterLiveListenerEnds(t *testing.T) {
	t.Chdir(t.TempDir())
	path := "g.sock"
	first, err := Listen("unix://" + path)
	if err != nil {
		t.Fatal(err)
	}
	go serveOnce(first)
	if l, err := Listen("unix://" + path); err == nil {
		l.Close()
		t.Fatal("live listener lost its path")
	}
	// A crashed helper leaves its socket file behind.
	first.(*net.UnixListener).SetUnlinkOnClose(false)
	first.Close()
	second, err := Listen("unix://" + path)
	if err != nil {
		t.Fatalf("stale socket was not recovered: %v", err)
	}
	defer second.Close()
	go serveOnce(second)
	requireAnswers(t, path)
}

func TestListenUnixBindErrorOmitsPath(t *testing.T) {
	t.Chdir(t.TempDir())
	// The parent directory does not exist, so the pre-bind cleanup passes
	// and the bind itself fails.
	const spec = "unix://no-such-dir-secret/guest-secret.sock"
	l, err := Listen(spec)
	if err == nil {
		l.Close()
		t.Fatal("bind in a missing directory succeeded")
	}
	if !errors.Is(err, ErrBindFailed) {
		t.Fatalf("err = %v, want ErrBindFailed", err)
	}
	if runtime.GOOS == "windows" {
		// Winsock reports a bind in a missing directory with an error number
		// (for example "dead network") that does not map to os.ErrNotExist,
		// so only require that the original OS error number and its text
		// are kept.
		var errno syscall.Errno
		if !errors.As(err, &errno) {
			t.Fatalf("err = %v does not keep the OS cause", err)
		}
		if !strings.HasSuffix(err.Error(), ": "+errno.Error()) {
			t.Fatalf("err = %v does not carry the text of the OS cause %v", err, errno)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v does not keep the OS cause", err)
	}
	for _, leak := range []string{"no-such-dir-secret", "guest-secret", ".sock"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("error contains %q: %v", leak, err)
		}
	}
	if !strings.Contains(err.Error(), "bind unix socket") {
		t.Fatalf("error does not name the operation: %v", err)
	}
}

func TestBindErrorMapsAddressInUse(t *testing.T) {
	err := bindError(&net.OpError{Op: "listen", Net: "unix", Addr: &net.UnixAddr{Name: "secret.sock", Net: "unix"}, Err: os.NewSyscallError("bind", syscall.EADDRINUSE)})
	if !errors.Is(err, ErrAddressInUse) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v", err)
	}
}

func TestIsAddrInUse(t *testing.T) {
	winsock := os.NewSyscallError("bind", windowsAddrInUse)
	for _, tc := range []struct {
		name string
		err  error
		goos string
		want bool
	}{
		{"EADDRINUSE on linux", syscall.EADDRINUSE, "linux", true},
		{"EADDRINUSE on windows", syscall.EADDRINUSE, "windows", true},
		{"Winsock 10048 on windows", winsock, "windows", true},
		{"wrapped Winsock 10048 on windows", &net.OpError{Op: "listen", Net: "unix", Err: winsock}, "windows", true},
		{"Winsock 10048 on linux", winsock, "linux", false},
		{"Winsock 10048 on darwin", winsock, "darwin", false},
		{"other errno on windows", os.NewSyscallError("bind", syscall.Errno(10013)), "windows", false},
		{"non-errno error on windows", errors.New("other"), "windows", false},
	} {
		if got := isAddrInUse(tc.err, tc.goos); got != tc.want {
			t.Errorf("%s: isAddrInUse = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestBindErrorWinsockAddrInUseOnWindowsOnly(t *testing.T) {
	err := bindError(&net.OpError{Op: "listen", Net: "unix", Addr: &net.UnixAddr{Name: "secret.sock", Net: "unix"}, Err: os.NewSyscallError("bind", windowsAddrInUse)})
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("error exposes the path: %v", err)
	}
	if runtime.GOOS == "windows" {
		if !errors.Is(err, ErrAddressInUse) {
			t.Fatalf("err = %v, want ErrAddressInUse", err)
		}
		return
	}
	if !errors.Is(err, ErrBindFailed) || errors.Is(err, ErrAddressInUse) {
		t.Fatalf("err = %v, want ErrBindFailed", err)
	}
}

func TestListenUnixProbeFailureKeepsSocketFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the mode bits of a socket file do not restrict a connection on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the socket file permission")
	}
	t.Chdir(t.TempDir())
	path := "g.sock"
	first, err := Listen("unix://" + path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	go serveOnce(first)
	if err := os.Chmod(path, 0); err != nil {
		t.Skipf("chmod unsupported: %v", err)
	}
	// connect(2) on a unix socket needs write permission, so the probe
	// fails with EACCES, which does not prove the file stale.
	l, err := Listen("unix://" + path)
	if err == nil {
		l.Close()
		t.Fatal("listener replaced a socket file that the probe could not check")
	}
	if !errors.Is(err, ErrProbeFailed) || strings.Contains(err.Error(), path) {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Lstat(path); statErr != nil {
		t.Fatalf("socket file removed: %v", statErr)
	}
}
