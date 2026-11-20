//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/netpolicy"
)

// policyRefresh is how often the agent re-reads the host addresses and
// routes and updates the rules when they changed.
const policyRefresh = 10 * time.Second

// network owns the agent's nftables table and the sandbox tap devices. It
// needs CAP_NET_ADMIN, the `ip` (iproute2) and `nft` tools, and IPv4
// forwarding.
type network struct {
	log   *slog.Logger
	rng   netip.Prefix
	mu    sync.Mutex
	last  string
	err   error
	ready bool
}

func run(ctx context.Context, name string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// hostNetwork reads the host addresses and routes of every routing table.
func hostNetwork(ctx context.Context) (netpolicy.Host, error) {
	var h netpolicy.Host
	out, err := run(ctx, "ip", nil, "-json", "address", "show")
	if err != nil {
		return h, err
	}
	if h.Addresses, err = netpolicy.ParseAddresses(out); err != nil {
		return h, err
	}
	for _, v6 := range []bool{false, true} {
		args := []string{"-json", "route", "show", "table", "all"}
		if v6 {
			args = append([]string{"-6"}, args...)
		} else {
			args = append([]string{"-4"}, args...)
		}
		out, err := run(ctx, "ip", nil, args...)
		if err != nil {
			return h, err
		}
		rs, err := netpolicy.ParseRoutes(out, v6)
		if err != nil {
			return h, err
		}
		h.Routes = append(h.Routes, rs...)
	}
	return h, nil
}

// checkGuestRange fails when a host address or route outside the sandbox
// taps overlaps the guest range.
func checkGuestRange(h netpolicy.Host, rng netip.Prefix) error {
	for _, a := range h.Addresses {
		if !strings.HasPrefix(a.Interface, netpolicy.SandboxInterfacePrefix) && a.Prefix.Masked().Overlaps(rng) {
			return fmt.Errorf("host interface %s uses the guest range %s", a.Interface, rng)
		}
	}
	for _, r := range h.Routes {
		if !strings.HasPrefix(r.Interface, netpolicy.SandboxInterfacePrefix) && !netpolicy.IsDefault(r.Prefix) && r.Prefix.Bits() > 1 && r.Prefix.Overlaps(rng) {
			return fmt.Errorf("a host route through %s overlaps the guest range %s", r.Interface, rng)
		}
	}
	return nil
}

// apply computes the ruleset from the current host network and applies it
// when it changed.
func (n *network) apply(ctx context.Context) error {
	h, err := hostNetwork(ctx)
	if err == nil {
		err = checkGuestRange(h, n.rng)
	}
	if err != nil {
		n.setErr(err)
		return err
	}
	v4, v6 := netpolicy.Blocked(h)
	rs := netpolicy.Ruleset(netpolicy.Config{GuestRange: n.rng, Blocked4: v4, Blocked6: v6})
	n.mu.Lock()
	same := rs == n.last && n.ready
	n.mu.Unlock()
	if same {
		return nil
	}
	if _, err := run(ctx, "nft", []byte(rs), "-f", "-"); err != nil {
		n.setErr(err)
		n.log.Error("network policy setup failed", "error", err)
		return err
	}
	n.mu.Lock()
	n.last, n.ready, n.err = rs, true, nil
	n.mu.Unlock()
	n.log.Info("network policy applied", "blocked_ipv4_prefixes", len(v4), "blocked_ipv6_prefixes", len(v6))
	return nil
}

func (n *network) setErr(err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.err, n.ready = err, false
}

// status returns nil when the policy is applied.
func (n *network) status() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ready {
		return nil
	}
	if n.err != nil {
		return n.err
	}
	return errors.New("network policy is not applied yet")
}

// watch re-applies the policy when interfaces, addresses or routes change.
func (n *network) watch(ctx context.Context) {
	t := time.NewTicker(policyRefresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = n.apply(ctx)
		}
	}
}

// setup enables IPv4 forwarding, removes leftover taps and applies the
// policy.
func (n *network) setup(ctx context.Context) error {
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0o644); err != nil {
		n.setErr(fmt.Errorf("enable IPv4 forwarding: %w", err))
		return n.status()
	}
	n.removeLeftoverTaps(ctx)
	return n.apply(ctx)
}

func (n *network) removeLeftoverTaps(ctx context.Context) {
	out, err := run(ctx, "ip", nil, "-o", "link", "show")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		name := strings.TrimSuffix(strings.SplitN(f[1], "@", 2)[0], ":")
		if strings.HasPrefix(name, netpolicy.SandboxInterfacePrefix) {
			_, _ = run(ctx, "ip", nil, "link", "delete", name)
			n.log.Info("removed a leftover sandbox tap device", "tap", name)
		}
	}
}

// addTap creates the tap device of a link with the gateway address.
func (n *network) addTap(ctx context.Context, l Link) error {
	_, _ = run(ctx, "ip", nil, "link", "delete", l.Tap)
	steps := [][]string{
		{"tuntap", "add", "dev", l.Tap, "mode", "tap"},
		{"address", "add", netip.PrefixFrom(l.Gateway, 30).String(), "dev", l.Tap},
		{"link", "set", l.Tap, "up"},
	}
	for _, s := range steps {
		if _, err := run(ctx, "ip", nil, s...); err != nil {
			_, _ = run(ctx, "ip", nil, "link", "delete", l.Tap)
			return err
		}
	}
	return nil
}

func (n *network) deleteTap(ctx context.Context, tap string) {
	_, _ = run(ctx, "ip", nil, "link", "delete", tap)
}

// teardown deletes the agent table.
func (n *network) teardown(ctx context.Context) {
	_, _ = run(ctx, "nft", nil, "delete", "table", "inet", netpolicy.TableName)
}
