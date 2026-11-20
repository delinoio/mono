// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package credstore

import (
	"fmt"
	"os"
)

func restrictDir(dir string) error {
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("credstore: restrict directory: %w", err)
	}
	return nil
}

func restrictFile(f *os.File, _ string) error {
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("credstore: restrict file: %w", err)
	}
	return nil
}
