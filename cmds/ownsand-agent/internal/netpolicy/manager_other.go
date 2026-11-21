// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package netpolicy

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
)

// errUnsupported: the nftables policy exists on Linux hosts only. The macOS
// (userspace network stack) and Windows (HNS ACL or WFP) enforcement are
// open decisions of the agent contract.
var errUnsupported = errors.New("netpolicy: the OUTBOUND policy is implemented for Linux hosts only")

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

// HostRoutes fails on this host OS.
func HostRoutes() ([]Route, []netip.Prefix, error) { return nil, nil, errUnsupported }

// CleanupHost does nothing on this host OS.
func CleanupHost(*slog.Logger) {}
