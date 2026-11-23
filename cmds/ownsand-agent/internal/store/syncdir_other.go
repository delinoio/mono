// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !linux

package store

// syncDirectory does nothing outside Linux. Flushing the directory entry of
// the pending identity is a Linux guarantee only: Windows has no directory
// fsync, and macOS stays unchanged until its own durability behavior is
// decided and validated. Remove this no-op for a platform when that
// platform gets its own implementation.
func syncDirectory(string) error { return nil }
