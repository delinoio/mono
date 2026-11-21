// SPDX-License-Identifier: Apache-2.0

//go:build !darwin && !windows

package main

import "log/slog"

// growSystemDisk: Linux guests grow their root file system in the init
// process (see setup_linux.go), so the flag changes nothing here.
func growSystemDisk(logger *slog.Logger) {
	logger.Debug("--grow-disk has no effect on this guest OS; the init process grows the root file system")
}
