// SPDX-License-Identifier: Apache-2.0

package netpolicy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// RefreshInterval is how often the agent recomputes the blocked prefixes
// from the routing table and the interface addresses, so it follows
// interface, address and route changes.
const RefreshInterval = 30 * time.Second

type attachment struct {
	link Link
	tap  string
	dns  *DNSForwarder
}

// Manager attaches OUTBOUND sandboxes to tap devices and keeps the
// nftables table of the agent current. It needs CAP_NET_ADMIN and the `ip`
// (iproute2) and `nft` commands.
type Manager struct {
	logger   *slog.Logger
	mu       sync.Mutex
	attached map[string]*attachment
	used     map[int]bool
	rules    string
}

// NewManager returns a manager.
func NewManager(logger *slog.Logger) *Manager {
	return &Manager{logger: logger, attached: map[string]*attachment{}, used: map[int]bool{}}
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("netpolicy: %s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// HostRoutes reads the IPv4 and IPv6 routes and the interface prefixes of
// the host, without the agent's own tap devices.
func HostRoutes() ([]Route, []netip.Prefix, error) {
	var routes []Route
	for _, src := range []struct {
		path  string
		parse func(r *os.File) ([]Route, error)
	}{
		{"/proc/net/route", func(f *os.File) ([]Route, error) { return ParseProcRoute(f) }},
		{"/proc/net/ipv6_route", func(f *os.File) ([]Route, error) { return ParseProcIPv6Route(f) }},
	} {
		f, err := os.Open(src.path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		rs, err := src.parse(f)
		f.Close()
		if err != nil {
			return nil, nil, err
		}
		routes = append(routes, rs...)
	}
	var onLink []netip.Prefix
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, nil, err
	}
	for _, iface := range ifaces {
		if strings.HasPrefix(iface.Name, TapPrefix) {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				if p, err := netip.ParsePrefix(ipn.String()); err == nil {
					onLink = append(onLink, p.Masked())
				}
			}
		}
	}
	return routes, onLink, nil
}

// Refresh recomputes the ruleset and applies it when it changed.
func (m *Manager) Refresh() error {
	routes, onLink, err := HostRoutes()
	if err != nil {
		return err
	}
	v4, v6 := Blocked(routes, onLink)
	rules := Ruleset(v4, v6)
	m.mu.Lock()
	defer m.mu.Unlock()
	if rules == m.rules {
		return nil
	}
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(rules)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("netpolicy: apply nftables rules: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	m.rules = rules
	m.logger.Info("network policy applied", "blocked_ipv4_prefixes", len(v4), "blocked_ipv6_prefixes", len(v6))
	return nil
}

// Attach creates the tap device of an OUTBOUND sandbox, applies the
// policy and starts its DNS forwarder.
func (m *Manager) Attach(sandboxID string) (Link, string, error) {
	m.mu.Lock()
	idx := 0
	for i := 1; i < 1<<14; i++ {
		if !m.used[i] {
			idx = i
			break
		}
	}
	if idx == 0 {
		m.mu.Unlock()
		return Link{}, "", errNoLinks
	}
	m.used[idx] = true
	m.mu.Unlock()
	link, _ := LinkFor(idx)
	tap := TapName(sandboxID)
	fail := func(err error) (Link, string, error) {
		_ = run("ip", "link", "del", tap)
		m.mu.Lock()
		delete(m.used, idx)
		m.mu.Unlock()
		m.logger.Error("network setup failed", "sandbox_id", sandboxID, "error", err)
		return Link{}, "", err
	}
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0o644); err != nil {
		return fail(fmt.Errorf("netpolicy: enable IPv4 forwarding: %w", err))
	}
	_ = run("ip", "link", "del", tap)
	for _, args := range [][]string{
		{"tuntap", "add", "dev", tap, "mode", "tap"},
		{"addr", "add", fmt.Sprintf("%s/%d", link.Gateway, link.Bits), "dev", tap},
		{"link", "set", tap, "up"},
	} {
		if err := run("ip", args...); err != nil {
			return fail(err)
		}
	}
	if err := m.Refresh(); err != nil {
		return fail(err)
	}
	dns, err := StartDNS(netip.AddrPortFrom(link.Gateway, 53), HostResolver("/etc/resolv.conf"), m.logger)
	if err != nil {
		return fail(fmt.Errorf("netpolicy: start DNS forwarder: %w", err))
	}
	m.mu.Lock()
	m.attached[sandboxID] = &attachment{link: link, tap: tap, dns: dns}
	m.mu.Unlock()
	m.logger.Info("network setup finished", "sandbox_id", sandboxID, "tap", tap)
	return link, tap, nil
}

// Detach removes the tap device and DNS forwarder of a sandbox.
func (m *Manager) Detach(sandboxID string) {
	m.mu.Lock()
	a, ok := m.attached[sandboxID]
	delete(m.attached, sandboxID)
	if ok {
		delete(m.used, a.link.Index)
	}
	m.mu.Unlock()
	if !ok {
		return
	}
	a.dns.Close()
	if err := run("ip", "link", "del", a.tap); err != nil {
		m.logger.Warn("delete tap device failed", "sandbox_id", sandboxID, "error", err)
	}
}

// Run refreshes the policy every RefreshInterval while sandboxes are
// attached.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(RefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.mu.Lock()
			n := len(m.attached)
			m.mu.Unlock()
			if n > 0 {
				if err := m.Refresh(); err != nil {
					m.logger.Error("network policy refresh failed", "error", err)
				}
			}
		}
	}
}

// CleanupHost deletes every sandbox tap device and the agent's nftables
// table, for an agent start (the VMs ended with the previous process) and
// for `ownsand-agent reset`.
func CleanupHost(logger *slog.Logger) {
	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
		if strings.HasPrefix(iface.Name, TapPrefix) {
			if err := run("ip", "link", "del", iface.Name); err != nil {
				logger.Warn("delete stale tap device failed", "tap", iface.Name, "error", err)
			}
		}
	}
	_ = run("nft", "delete", "table", "inet", TableName)
}
