//go:build !linux

package guest

import (
	"errors"
	"os"
)

// reflink is not available; the caller copies the file.
func reflink(_, _ *os.File) error { return errors.New("reflink is not supported on this guest OS") }
