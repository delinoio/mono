// SPDX-License-Identifier: Apache-2.0

package guestlisten

import (
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
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
