// SPDX-License-Identifier: FSL-1.1-ALv2

package netpolicy

import (
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDNSForwarder(t *testing.T) {
	// Upstream: UDP and TCP echo with a marker.
	up, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.MustParseAddrPort("127.0.0.1:0")))
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := up.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			up.WriteToUDPAddrPort(append([]byte("ok:"), buf[:n]...), from)
		}
	}()
	upTCP, err := net.Listen("tcp", up.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer upTCP.Close()
	go func() {
		for {
			c, err := upTCP.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				var l [2]byte
				io.ReadFull(c, l[:])
				msg := make([]byte, binary.BigEndian.Uint16(l[:]))
				io.ReadFull(c, msg)
				c.Write(append(l[:], msg...))
			}()
		}
	}()
	f, err := StartDNS(netip.MustParseAddrPort("127.0.0.1:0"), up.LocalAddr().String(), slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c, err := net.Dial("udp", f.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write([]byte("query"))
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	if err != nil || string(buf[:n]) != "ok:query" {
		t.Fatalf("udp answer %q %v", buf[:n], err)
	}
	tc, err := net.Dial("tcp", f.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	tc.SetDeadline(time.Now().Add(5 * time.Second))
	tc.Write([]byte{0, 3, 'a', 'b', 'c'})
	got := make([]byte, 5)
	if _, err := io.ReadFull(tc, got); err != nil || string(got[2:]) != "abc" {
		t.Fatalf("tcp answer %q %v", got, err)
	}
}

func TestHostResolver(t *testing.T) {
	p := filepath.Join(t.TempDir(), "resolv.conf")
	os.WriteFile(p, []byte("# x\nsearch lan\nnameserver 192.0.2.53\nnameserver 192.0.2.54\n"), 0o644)
	if got := HostResolver(p); got != "192.0.2.53:53" {
		t.Fatalf("resolver %s", got)
	}
	for _, c := range []struct{ conf, want string }{
		{"nameserver fe80::53%eth-test\nnameserver 192.0.2.54\n", "[fe80::53%eth-test]:53"},
		{"nameserver 2001:db8::53\nnameserver 192.0.2.54\n", "[2001:db8::53]:53"},
	} {
		os.WriteFile(p, []byte(c.conf), 0o644)
		if got := HostResolver(p); got != c.want {
			t.Fatalf("resolver %q: got %s want %s", c.conf, got, c.want)
		}
	}
	if got := HostResolver(filepath.Join(t.TempDir(), "missing")); got != "127.0.0.53:53" {
		t.Fatalf("fallback %s", got)
	}
}
