// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !linux

package processrt

import "path/filepath"

func socketPath(dir string, _ string) string {
	return filepath.Join(dir, "g.sock")
}
