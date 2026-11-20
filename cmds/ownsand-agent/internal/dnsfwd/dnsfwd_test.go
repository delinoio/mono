package dnsfwd

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

// upstream echoes every UDP datagram and every length-prefixed TCP message
// with the first byte changed.
func upstream(t *testing.T) string {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", pc.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close(); _ = ln.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			buf[0] = 'R'
			_, _ = pc.WriteTo(buf[:n], from)
		}
	}()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				var l [2]byte
				if _, err := io.ReadFull(c, l[:]); err != nil {
					return
				}
				msg := make([]byte, binary.BigEndian.Uint16(l[:]))
				_, _ = io.ReadFull(c, msg)
				msg[0] = 'R'
				_, _ = c.Write(append(l[:], msg...))
			}()
		}
	}()
	return pc.LocalAddr().String()
}

func TestForwardsUDPAndTCP(t *testing.T) {
	up := upstream(t)
	f, err := Start(netip.MustParseAddrPort("127.0.0.1:0"), []string{up}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c, err := net.Dial("udp", f.UDPAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = c.Write([]byte("Qquery"))
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	if err != nil || string(buf[:n]) != "Rquery" {
		t.Fatalf("udp answer %q %v", buf[:n], err)
	}
	// The TCP listener uses another ephemeral port than UDP in this test.
	tc, err := net.Dial("tcp", f.TCPAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	_ = tc.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = tc.Write([]byte{0, 3, 'Q', 'a', 'b'})
	resp := make([]byte, 5)
	if _, err := io.ReadFull(tc, resp); err != nil || string(resp[2:]) != "Rab" {
		t.Fatalf("tcp answer %q %v", resp, err)
	}
}

func TestUpstreamsFromResolvConf(t *testing.T) {
	p := filepath.Join(t.TempDir(), "resolv.conf")
	_ = os.WriteFile(p, []byte("# c\nnameserver 127.0.0.53\nnameserver fe80::1%eth0\nnameserver bad\nsearch x\n"), 0o644)
	got := Upstreams(p)
	if len(got) != 2 || got[0] != "127.0.0.53:53" || got[1] != "[fe80::1%eth0]:53" {
		t.Fatalf("upstreams %v", got)
	}
	if _, err := Start(netip.MustParseAddrPort("127.0.0.1:0"), nil, slog.Default()); err == nil {
		t.Fatal("start without upstreams must fail")
	}
}
