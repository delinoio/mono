// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !windows

package store

import (
	"fmt"
	"os"
	"path/filepath"
)

// PrepareDir creates the state directory and limits access to its owner.
func PrepareDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("store: create %s: %w", dir, err)
	}
	return restrictStateDir(dir)
}

func prepareStateLayout(dir string) error {
	root := filepath.Clean(dir)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("store: create %s: %w", root, err)
	}
	for _, sub := range stateSubdirs {
		path := filepath.Join(dir, sub)
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("store: create %s: %w", path, err)
		}
	}
	return restrictStateDir(dir)
}

// The directory may exist with wider permissions from an operator.
func restrictStateDir(dir string) error {
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("store: restrict state directory: %w", err)
	}
	return nil
}

func prepareResetDir(dir string) (func(), error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("store: stat state directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("store: state directory %q is not a directory", dir)
	}
	return func() {}, nil
}
