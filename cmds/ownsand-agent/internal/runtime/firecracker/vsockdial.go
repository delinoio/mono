package firecracker

import (
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// Handshake runs the host-initiated vsock connection protocol of Firecracker
// on a connection to the vsock Unix socket: the host writes
// "CONNECT <port>\n", and Firecracker answers "OK <host port>\n" when the
// guest accepted the connection. It reads byte by byte, so no guest data is
// consumed.
func Handshake(c net.Conn, port uint32, timeout time.Duration) error {
	_ = c.SetDeadline(time.Now().Add(timeout))
	defer c.SetDeadline(time.Time{})
	if _, err := fmt.Fprintf(c, "CONNECT %d\n", port); err != nil {
		return err
	}
	var line []byte
	b := make([]byte, 1)
	for len(line) < 64 {
		if _, err := io.ReadFull(c, b); err != nil {
			return fmt.Errorf("vsock handshake: %w", err)
		}
		if b[0] == '\n' {
			break
		}
		line = append(line, b[0])
	}
	if !strings.HasPrefix(string(line), "OK ") {
		return fmt.Errorf("vsock handshake refused")
	}
	return nil
}
