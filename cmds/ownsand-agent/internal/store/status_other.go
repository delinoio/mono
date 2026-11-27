// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !windows

package store

import (
	"context"
	"os"
)

func writeStatusFile(_ context.Context, path string, data []byte) error {
	return WriteFileAtomic(path, data, 0o600)
}

func readStatusFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}
