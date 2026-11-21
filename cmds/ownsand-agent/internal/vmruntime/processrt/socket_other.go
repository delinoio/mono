// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package processrt

import "path/filepath"

func socketPath(dir string, _ string) string {
	return filepath.Join(dir, "g.sock")
}
