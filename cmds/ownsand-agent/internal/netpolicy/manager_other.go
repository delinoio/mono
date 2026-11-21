// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !linux

package netpolicy

import (
	"context"
	"errors"
	"log/slog"
)

// errUnsupported: the nftables policy exists on Linux hosts only. macOS and
// Windows hosts enforce the policy in the userspace network stack of
// package netstack with a Filter.
var errUnsupported = errors.New("netpolicy: the nftables policy exists on Linux hosts only")

// Manager is not available on this host OS.
type Manager struct{}

// NewManager returns a manager whose operations fail.
func NewManager(*slog.Logger) *Manager { return &Manager{} }

// Attach fails on this host OS.
func (m *Manager) Attach(string) (Link, string, error) { return Link{}, "", errUnsupported }

// Detach does nothing on this host OS.
func (m *Manager) Detach(string) {}

// Refresh fails on this host OS.
func (m *Manager) Refresh() error { return errUnsupported }

// Run does nothing on this host OS.
func (m *Manager) Run(context.Context) {}

// CleanupHost does nothing on this host OS.
func CleanupHost(*slog.Logger) {}
