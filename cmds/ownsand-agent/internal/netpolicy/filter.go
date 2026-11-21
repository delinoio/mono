// SPDX-License-Identifier: Apache-2.0

package netpolicy

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"
)

// RefreshInterval is how often the agent recomputes the blocked prefixes
// from the routing table and the interface addresses, so it follows
// interface, address and route changes.
const RefreshInterval = 30 * time.Second

// InterfacePrefixes returns the masked prefixes of the addresses of every
// host interface whose name skip does not reject.
func InterfacePrefixes(skip func(name string) bool) ([]netip.Prefix, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []netip.Prefix
	for _, iface := range ifaces {
		if skip != nil && skip(iface.Name) {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				if p, err := netip.ParsePrefix(ipn.String()); err == nil {
					out = append(out, p)
				}
			}
		}
	}
	return out, nil
}

// Filter decides which guest destinations the OUTBOUND policy allows, for
// the userspace network stack of the macOS and Windows runtimes (package
// netstack), which opens every guest connection from the agent. It blocks
// the prefixes of Blocked for the current host routes and interface
// prefixes, and every address of the host. A Filter is safe for concurrent
// use.
type Filter struct {
	logger *slog.Logger
	// routes reads the host routes and interface prefixes; tests replace it.
	routes func() ([]Route, []netip.Prefix, error)

	mu    sync.RWMutex
	v4    []netip.Prefix
	v6    []netip.Prefix
	host  map[netip.Addr]bool
	valid bool
}

// NewFilter returns a filter that blocks everything until its first
// successful Refresh.
func NewFilter(logger *slog.Logger) *Filter {
	return &Filter{logger: logger, routes: HostRoutes}
}

// Refresh recomputes the blocked prefixes and the host addresses. After a
// failure the filter keeps its last state (or keeps blocking everything
// before its first success).
func (f *Filter) Refresh() error {
	routes, onLink, err := f.routes()
	if err != nil {
		return err
	}
	host := map[netip.Addr]bool{}
	for _, p := range onLink {
		host[p.Addr().Unmap()] = true
	}
	masked := make([]netip.Prefix, 0, len(onLink))
	for _, p := range onLink {
		masked = append(masked, p.Masked())
	}
	v4, v6 := Blocked(routes, masked)
	f.mu.Lock()
	changed := !f.valid || !slices.Equal(v4, f.v4) || !slices.Equal(v6, f.v6)
	f.v4, f.v6, f.host, f.valid = v4, v6, host, true
	f.mu.Unlock()
	if changed {
		f.logger.Info("network policy filter updated", "blocked_ipv4_prefixes", len(v4), "blocked_ipv6_prefixes", len(v6), "host_addresses", len(host))
	}
	return nil
}

// Allows reports whether a guest may open a connection to addr.
func (f *Filter) Allows(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	if !f.valid {
		return false
	}
	if f.host[addr.Unmap()] {
		return false
	}
	// An IPv4 address also matches the IPv4-mapped and NAT64 prefixes
	// through the IPv4 list; an IPv6 address is checked in the IPv6 list,
	// which holds those embedded forms too.
	list := f.v6
	if addr.Is4() {
		list = f.v4
	}
	for _, p := range list {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

// Run refreshes the filter every RefreshInterval until ctx ends. It
// refreshes once at start.
func (f *Filter) Run(ctx context.Context) {
	if err := f.Refresh(); err != nil {
		f.logger.Error("network policy filter refresh failed", "error", err)
	}
	t := time.NewTicker(RefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := f.Refresh(); err != nil {
				f.logger.Error("network policy filter refresh failed", "error", err)
			}
		}
	}
}

// SetRoutesForTest replaces the host route reader of f.
func (f *Filter) SetRoutesForTest(routes func() ([]Route, []netip.Prefix, error)) {
	f.routes = routes
}
