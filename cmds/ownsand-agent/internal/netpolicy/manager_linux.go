// SPDX-License-Identifier: FSL-1.1-ALv2

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

type attachment struct {
	link Link
	tap  string
	dns  dnsCloser
}

// dnsCloser is the part of a DNS forwarder that the manager needs after the
// start; tests replace the forwarder.
type dnsCloser interface {
	Close()
}

// Manager attaches OUTBOUND sandboxes to tap devices and keeps the
// nftables table of the agent current. It needs CAP_NET_ADMIN and the `ip`
// (iproute2) and `nft` commands.
type Manager struct {
	logger   *slog.Logger
	mu       sync.Mutex
	attached map[string]*attachment
	used     map[int]bool
	// gateways holds the DNS exception of each tap (the key) that the
	// ruleset renders: Attach stages it before the first policy refresh of
	// the sandbox and removes it when the attachment fails or ends.
	gateways map[string]TapGateway
	rules    string
	// interval and refresh are RefreshInterval and Refresh; tests replace
	// them.
	interval time.Duration
	refresh  func() error
	// run, startDNS, setForwarding, tapExists, hostRoutes and applyRules are
	// the host operations of the manager: the `ip` command, the DNS
	// forwarder, the IPv4 forwarding switch, the tap device probe, the
	// routing table read and the `nft` script replacement. Tests replace
	// them so that the manager runs without root.
	run           func(name string, args ...string) error
	startDNS      func(listen netip.AddrPort, upstream string, logger *slog.Logger) (dnsCloser, error)
	setForwarding func() error
	tapExists     func(tap string) bool
	hostRoutes    func() ([]Route, []netip.Prefix, error)
	applyRules    func(rules string) error
}

// NewManager returns a manager.
func NewManager(logger *slog.Logger) *Manager {
	m := &Manager{
		logger:        logger,
		attached:      map[string]*attachment{},
		used:          map[int]bool{},
		gateways:      map[string]TapGateway{},
		interval:      RefreshInterval,
		run:           run,
		startDNS:      startDNSForwarder,
		setForwarding: enableIPv4Forwarding,
		tapExists:     tapDeviceExists,
		hostRoutes:    HostRoutes,
		applyRules:    applyNftRules,
	}
	m.refresh = m.Refresh
	return m
}

// startDNSForwarder is StartDNS behind the dnsCloser interface. It returns a
// nil interface, not a nil pointer, when the start fails.
func startDNSForwarder(listen netip.AddrPort, upstream string, logger *slog.Logger) (dnsCloser, error) {
	f, err := StartDNS(listen, upstream, logger)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func enableIPv4Forwarding() error {
	return os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0o644)
}

// errNoSuchInterface is the text of the error that net.InterfaceByName
// returns for a missing device; the standard library does not export the
// error value.
const errNoSuchInterface = "no such network interface"

// tapDeviceExists reports whether the tap device may still exist. It is
// false only when net.InterfaceByName confirms that the device is absent; an
// unknown lookup failure counts as "may exist", so that the caller keeps the
// reservation.
func tapDeviceExists(tap string) bool {
	_, err := net.InterfaceByName(tap)
	return err == nil || !strings.Contains(err.Error(), errNoSuchInterface)
}

// applyNftRules replaces the agent's nftables table with `nft -f`.
func applyNftRules(rules string) error {
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(rules)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("netpolicy: apply nftables rules: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("netpolicy: %s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// ipRouteDump runs `ip -4 -o route show table all` and returns its output.
// Tests replace it so that the route source runs without the `ip` command.
var ipRouteDump = func() ([]byte, error) {
	cmd := exec.Command("ip", "-4", "-o", "route", "show", "table", "all")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("netpolicy: ip -4 -o route show table all: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// HostRoutes reads the IPv4 and IPv6 routes and the interface prefixes of
// the host, without the agent's own tap devices. The IPv4 routes come from
// `ip -4 -o route show table all`, so routes of the policy routing tables
// count like routes of the main table; /proc/net/route lists only the main
// table. A failed route dump is an error: the caller keeps the rules it
// applied last and reports the failure.
func HostRoutes() ([]Route, []netip.Prefix, error) {
	dump, err := ipRouteDump()
	if err != nil {
		return nil, nil, err
	}
	routes, err := ParseIPRouteShow(bytes.NewReader(dump))
	if err != nil {
		return nil, nil, err
	}
	f, err := os.Open("/proc/net/ipv6_route")
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, nil, err
	default:
		rs, err := ParseProcIPv6Route(f)
		f.Close()
		if err != nil {
			return nil, nil, err
		}
		routes = append(routes, rs...)
	}
	onLink, err := InterfacePrefixes(func(name string) bool { return strings.HasPrefix(name, TapPrefix) })
	if err != nil {
		return nil, nil, err
	}
	return routes, onLink, nil
}

// Refresh recomputes the ruleset from the host routes and the current DNS
// assignments of the attached sandboxes and applies it when it changed.
func (m *Manager) Refresh() error {
	routes, onLink, err := m.hostRoutes()
	if err != nil {
		return err
	}
	v4, v6 := Blocked(routes, onLink)
	m.mu.Lock()
	defer m.mu.Unlock()
	assignments := make([]TapGateway, 0, len(m.gateways))
	for _, g := range m.gateways {
		assignments = append(assignments, g)
	}
	rules := Ruleset(v4, v6, assignments)
	if rules == m.rules {
		return nil
	}
	if err := m.applyRules(rules); err != nil {
		return err
	}
	m.rules = rules
	m.logger.Info("network policy applied", "blocked_ipv4_prefixes", len(v4), "blocked_ipv6_prefixes", len(v6), "dns_assignments", len(assignments))
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
	tap := TapName(idx)
	// applied is true once a refresh succeeded with this sandbox's DNS
	// assignment staged, so that the ruleset may hold its exception.
	applied := false
	fail := func(err error) (Link, string, error) {
		// A failed attach leaves no DNS exception: unstage the assignment
		// and, when a refresh already applied it, apply the ruleset
		// without it.
		m.unstage(tap)
		if applied {
			if rerr := m.refresh(); rerr != nil {
				m.logger.Error("network policy refresh after failed setup failed", "sandbox_id", sandboxID, "tap", tap, "error", rerr)
			}
		}
		// The link stays reserved while a tap device with the link address
		// may still exist.
		m.removeTap(sandboxID, tap, idx)
		m.logger.Error("network setup failed", "sandbox_id", sandboxID, "error", err)
		return Link{}, "", err
	}
	if err := m.setForwarding(); err != nil {
		return fail(fmt.Errorf("netpolicy: enable IPv4 forwarding: %w", err))
	}
	_ = m.run("ip", "link", "del", tap)
	for _, args := range [][]string{
		{"tuntap", "add", "dev", tap, "mode", "tap"},
		{"addr", "add", fmt.Sprintf("%s/%d", link.Gateway, link.Bits), "dev", tap},
		{"link", "set", tap, "up"},
	} {
		if err := m.run("ip", args...); err != nil {
			return fail(err)
		}
	}
	m.mu.Lock()
	m.gateways[tap] = TapGateway{Tap: tap, Gateway: link.Gateway}
	m.mu.Unlock()
	if err := m.refresh(); err != nil {
		return fail(err)
	}
	applied = true
	dns, err := m.startDNS(netip.AddrPortFrom(link.Gateway, 53), HostResolver("/etc/resolv.conf"), m.logger)
	if err != nil {
		return fail(fmt.Errorf("netpolicy: start DNS forwarder: %w", err))
	}
	m.mu.Lock()
	m.attached[sandboxID] = &attachment{link: link, tap: tap, dns: dns}
	m.mu.Unlock()
	m.logger.Info("network setup finished", "sandbox_id", sandboxID, "tap", tap)
	return link, tap, nil
}

// Detach removes the tap device and DNS forwarder of a sandbox. The link of
// the sandbox stays reserved until the DNS forwarder is closed and the tap
// device is deleted or confirmed absent, so that a concurrent Attach cannot
// receive the same /30 link and gateway address while they exist. The
// teardown runs outside the manager mutex, so other free links serve new
// sandboxes meanwhile.
func (m *Manager) Detach(sandboxID string) {
	m.mu.Lock()
	a, ok := m.attached[sandboxID]
	delete(m.attached, sandboxID)
	if ok {
		delete(m.gateways, a.tap)
	}
	m.mu.Unlock()
	if !ok {
		return
	}
	// Remove the DNS exception of the tap first, so that a reused tap name
	// never inherits a stale gateway rule. Other taps keep their rules.
	if err := m.refresh(); err != nil {
		m.logger.Error("network policy refresh after detach failed", "sandbox_id", sandboxID, "tap", a.tap, "error", err)
	}
	a.dns.Close()
	m.removeTap(sandboxID, a.tap, a.link.Index)
}

// unstage removes the DNS assignment of a tap from the next ruleset.
func (m *Manager) unstage(tap string) {
	m.mu.Lock()
	delete(m.gateways, tap)
	m.mu.Unlock()
}

// removeTap deletes a tap device and releases its link index. When the
// deletion fails and the device may still exist, the index stays reserved
// (the agent has no retry for it; CleanupHost removes the device at the next
// agent start) and the manager logs a warning.
func (m *Manager) removeTap(sandboxID, tap string, idx int) {
	err := m.run("ip", "link", "del", tap)
	if err != nil && m.tapExists(tap) {
		m.logger.Warn("delete tap device failed", "sandbox_id", sandboxID, "tap", tap, "error", err)
		return
	}
	if err != nil {
		m.logger.Debug("tap device already absent", "sandbox_id", sandboxID, "tap", tap, "error", err)
	}
	m.mu.Lock()
	delete(m.used, idx)
	m.mu.Unlock()
}

// Run refreshes the policy every RefreshInterval while sandboxes are
// attached, until ctx ends. The runtime that owns the manager runs it for
// its lifetime.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(m.interval)
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
				if err := m.refresh(); err != nil {
					m.logger.Error("network policy refresh failed", "error", err)
				}
			}
		}
	}
}

// hostCleanupOps are the host operations of CleanupHost; tests replace them
// so that the clean-up runs without root and without touching the host.
type hostCleanupOps struct {
	// listTaps returns the names of the sandbox tap devices of the host.
	listTaps func() ([]string, error)
	// run executes `ip` and `nft`.
	run func(name string, args ...string) error
	// tapExists reports whether a tap device may still exist.
	tapExists func(tap string) bool
}

// listTapDevices returns the names of the interfaces that start with
// TapPrefix.
func listTapDevices() ([]string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var taps []string
	for _, iface := range ifaces {
		if strings.HasPrefix(iface.Name, TapPrefix) {
			taps = append(taps, iface.Name)
		}
	}
	return taps, nil
}

// isNftTableAbsent reports whether an `nft delete table` failure means that
// the table does not exist (nft prints "No such file or directory" for a
// missing table), or that nft is not installed, so that the host cannot hold
// a table of the agent that the agent could delete.
func isNftTableAbsent(err error) bool {
	if errors.Is(err, exec.ErrNotFound) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such file or directory") || strings.Contains(msg, "does not exist")
}

// CleanupHost deletes every sandbox tap device and the agent's nftables
// table, for an agent start (the VMs ended with the previous process) and
// for `ownsand-agent reset`. It attempts every resource even when an earlier
// one fails, logs each failure with its operation and resource, and returns
// the failures joined (nil when every resource is deleted or was already
// absent, so a retry after a partial failure succeeds once the remaining
// resources are removable). The agent start treats the error as best effort;
// reset reports it.
func CleanupHost(logger *slog.Logger) error {
	return cleanupHost(logger, hostCleanupOps{listTaps: listTapDevices, run: run, tapExists: tapDeviceExists})
}

func cleanupHost(logger *slog.Logger, ops hostCleanupOps) error {
	var errs []error
	taps, err := ops.listTaps()
	if err != nil {
		logger.Warn("list tap devices failed", "operation", "list_taps", "error", err)
		errs = append(errs, fmt.Errorf("list tap devices: %w", err))
	}
	for _, tap := range taps {
		err := ops.run("ip", "link", "del", tap)
		switch {
		case err == nil:
		case !ops.tapExists(tap):
			logger.Debug("tap device already absent", "operation", "delete_tap", "tap", tap, "error", err)
		default:
			logger.Warn("delete stale tap device failed", "operation", "delete_tap", "tap", tap, "error", err)
			errs = append(errs, fmt.Errorf("delete tap device %s: %w", tap, err))
		}
	}
	if err := ops.run("nft", "delete", "table", "inet", TableName); err != nil {
		if isNftTableAbsent(err) {
			logger.Debug("nftables table already absent", "operation", "delete_nft_table", "table", TableName, "error", err)
		} else {
			logger.Warn("delete nftables table failed", "operation", "delete_nft_table", "table", TableName, "error", err)
			errs = append(errs, fmt.Errorf("delete nftables table inet %s: %w", TableName, err))
		}
	}
	return errors.Join(errs...)
}
