//go:build linux

package firecracker

import (
	"context"
	"fmt"
	"net"
	"syscall"
)

// oPath is O_PATH, which the syscall package does not define.
const oPath = 0x200000

// dialLongUnix connects to the socket name in dir through
// /proc/self/fd/<dir fd>/<name>, which stays short whatever the length of
// dir. It is used when the state directory makes a socket path longer than
// sun_path allows.
func dialLongUnix(ctx context.Context, dir, name string) (net.Conn, error) {
	fd, err := syscall.Open(dir, oPath|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(fd)
	var d net.Dialer
	return d.DialContext(ctx, "unix", fmt.Sprintf("/proc/self/fd/%d/%s", fd, name))
}
