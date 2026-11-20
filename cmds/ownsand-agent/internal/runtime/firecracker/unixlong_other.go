//go:build !linux

package firecracker

import (
	"context"
	"errors"
	"net"
)

func dialLongUnix(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("unix socket path is too long")
}
