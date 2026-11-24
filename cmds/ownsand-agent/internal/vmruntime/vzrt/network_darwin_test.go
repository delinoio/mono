// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build darwin && cgo

package vzrt

import (
	"errors"
	"net"
	"os"
	"testing"
	"time"
)

func TestNetworkDeviceClosesOriginalFile(t *testing.T) {
	t.Run("attached", func(t *testing.T) {
		vmFile, host, err := datagramPair()
		if err != nil {
			t.Fatal(err)
		}
		defer vmFile.Close()
		defer host.Close()

		// Keep a duplicate that the test can read and write directly.
		guest, err := net.FileConn(vmFile)
		if err != nil {
			t.Fatal(err)
		}
		defer guest.Close()

		if _, err := networkDevice(vmFile, MacOSGuestMAC); err != nil {
			t.Fatal(err)
		}
		if _, err := vmFile.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("original file after attachment: got %v, want os.ErrClosed", err)
		}

		deadline := time.Now().Add(time.Second)
		for _, conn := range []net.Conn{host, guest} {
			if err := conn.SetDeadline(deadline); err != nil {
				t.Fatal(err)
			}
		}
		for _, packet := range []struct {
			from, to net.Conn
			payload  string
		}{
			{host, guest, "host to guest"},
			{guest, host, "guest to host"},
		} {
			if _, err := packet.from.Write([]byte(packet.payload)); err != nil {
				t.Fatalf("write %q: %v", packet.payload, err)
			}
			var buf [32]byte
			n, err := packet.to.Read(buf[:])
			if err != nil {
				t.Fatalf("read %q: %v", packet.payload, err)
			}
			if got := string(buf[:n]); got != packet.payload {
				t.Fatalf("got %q, want %q", got, packet.payload)
			}
		}
	})

	t.Run("attachment failure", func(t *testing.T) {
		// A pipe is not a datagram socket, so the attachment rejects it.
		vmFile, peer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer vmFile.Close()
		defer peer.Close()

		if _, err := networkDevice(vmFile, MacOSGuestMAC); err == nil {
			t.Fatal("attachment accepted a pipe")
		}
		if _, err := vmFile.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("original file after attachment failure: got %v, want os.ErrClosed", err)
		}
	})
}
