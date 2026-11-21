// SPDX-License-Identifier: Apache-2.0

package main

import "log/slog"

// growSystemDisk extends the Windows partition to the end of its disk.
func growSystemDisk(logger *slog.Logger) {
	logger.Warn("--grow-disk is not implemented on Windows guests yet")
}
