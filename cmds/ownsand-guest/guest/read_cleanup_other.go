// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !windows

package guest

import (
	"log/slog"
	"os"
)

func clearReadRoot(root string, _ *slog.Logger) error {
	return os.RemoveAll(root)
}
