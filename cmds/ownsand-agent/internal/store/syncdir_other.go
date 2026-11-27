// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !linux

package store

// SyncPendingIdentityDirectories does nothing outside Linux. Windows has no
// directory fsync. Add macOS directory flushing when its durability behavior
// has been validated.
func (s *Store) SyncPendingIdentityDirectories() error { return nil }

func (s *Store) syncSandboxCleanupDirectory() error { return nil }
