// SPDX-License-Identifier: Apache-2.0

// Package syncpath resolves local sync sources for SandRun commands.
package syncpath

import (
	"path/filepath"
	"runtime"
	"strings"
)

// FromWorkDir anchors a sync source to workDir. An empty source uses workDir.
func FromWorkDir(workDir, source string) string {
	if source == "" {
		return workDir
	}
	if filepath.IsAbs(source) {
		return source
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(workDir, source)
	}
	// Unix resolves each component in order. Joining with filepath.Join could
	// erase a missing component or a symbolic link before a following "..".
	if workDir == "" || strings.HasSuffix(workDir, "/") {
		return workDir + source
	}
	return workDir + "/" + source
}
