// SPDX-License-Identifier: FSL-1.1-ALv2

package guestlisten

import (
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestVsockLoopback needs the vsock loopback transport (CID 1) of the host
// kernel; it skips when the host has none.
func TestVsockLoopback(t *testing.T) {
	l, err := listenVsock(40000 + uint32(os.Getpid()%10000))
	if err != nil {
		t.Skipf("vsock unavailable: %v", err)
	}
	defer l.Close()
	port := l.Addr().(*VsockAddr).Port
	done := make(chan []byte, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			done <- nil
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 5)
		n, _ := c.Read(buf)
		done <- buf[:n]
	}()
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Skipf("vsock socket: %v", err)
	}
	defer unix.Close(fd)
	if err := unix.Connect(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_LOCAL, Port: port}); err != nil {
		t.Skipf("vsock loopback connect: %v", err)
	}
	if _, err := unix.Write(fd, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if got := <-done; string(got) != "hello" {
		t.Fatalf("got %q", got)
	}
}
