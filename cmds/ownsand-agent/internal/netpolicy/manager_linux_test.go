// SPDX-License-Identifier: FSL-1.1-ALv2

package netpolicy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Run refreshes the policy periodically only while a sandbox is attached
// and returns when its context ends.
func TestRunRefreshesWhileAttached(t *testing.T) {
	m := NewManager(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	var n atomic.Int32
	m.interval = time.Millisecond
	m.refresh = func() error { n.Add(1); return nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()

	time.Sleep(20 * time.Millisecond)
	if got := n.Load(); got != 0 {
		t.Fatalf("%d refreshes without an attached sandbox", got)
	}
	m.mu.Lock()
	m.attached["s"] = &attachment{}
	m.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for n.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("no periodic refresh while a sandbox is attached")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// fakeHost replaces the host operations of a Manager. It records the `ip`
// commands, can block the DNS close and the tap deletion, and can fail the
// tap deletion while the tap still exists.
type fakeHost struct {
	mu          sync.Mutex
	cmds        []string
	dnsAddrs    []netip.AddrPort
	taps        map[string]bool
	delErr      error // returned by `ip link del` while set; the tap stays
	dnsClosing  chan struct{}
	dnsRelease  chan struct{} // when set, DNS Close blocks until closed
	delStarted  chan struct{}
	delRelease  chan struct{} // when set, `ip link del` blocks until closed
	logs        bytes.Buffer
	logger      *slog.Logger
	startDNSErr error
}

type fakeDNS struct {
	h *fakeHost
}

func (d fakeDNS) Close() {
	d.h.mu.Lock()
	closing, release := d.h.dnsClosing, d.h.dnsRelease
	d.h.mu.Unlock()
	if closing != nil {
		select {
		case closing <- struct{}{}:
		default:
		}
	}
	if release != nil {
		<-release
	}
}

func newFakeManager(t *testing.T) (*Manager, *fakeHost) {
	t.Helper()
	h := &fakeHost{taps: map[string]bool{}}
	h.logger = slog.New(slog.NewJSONHandler(&lockedWriter{h: h}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m := NewManager(h.logger)
	m.refresh = func() error { return nil }
	m.setForwarding = func() error { return nil }
	m.run = h.run
	m.startDNS = func(listen netip.AddrPort, _ string, _ *slog.Logger) (dnsCloser, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.startDNSErr != nil {
			return nil, h.startDNSErr
		}
		h.dnsAddrs = append(h.dnsAddrs, listen)
		return fakeDNS{h: h}, nil
	}
	m.tapExists = func(tap string) bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.taps[tap]
	}
	return m, h
}

type lockedWriter struct{ h *fakeHost }

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.h.mu.Lock()
	defer w.h.mu.Unlock()
	return w.h.logs.Write(p)
}

func (h *fakeHost) run(name string, args ...string) error {
	line := name + " " + strings.Join(args, " ")
	h.mu.Lock()
	h.cmds = append(h.cmds, line)
	h.mu.Unlock()
	if name != "ip" || len(args) == 0 {
		return nil
	}
	switch {
	case args[0] == "tuntap" && args[1] == "add":
		h.mu.Lock()
		h.taps[args[3]] = true
		h.mu.Unlock()
	case args[0] == "link" && args[1] == "del":
		tap := args[2]
		h.mu.Lock()
		started, release := h.delStarted, h.delRelease
		h.mu.Unlock()
		if started != nil {
			select {
			case started <- struct{}{}:
			default:
			}
		}
		if release != nil {
			<-release
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.delErr != nil {
			return h.delErr
		}
		if !h.taps[tap] {
			return errors.New("Cannot find device")
		}
		delete(h.taps, tap)
	}
	return nil
}

func (h *fakeHost) log() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.logs.String()
}

func mustAttach(t *testing.T, m *Manager, id string) Link {
	t.Helper()
	link, tap, err := m.Attach(id)
	if err != nil {
		t.Fatalf("Attach(%s): %v", id, err)
	}
	if tap != TapName(link.Index) {
		t.Fatalf("Attach(%s) tap = %q, want %q", id, tap, TapName(link.Index))
	}
	return link
}

func detachAsync(m *Manager, id string) <-chan struct{} {
	done := make(chan struct{})
	go func() { m.Detach(id); close(done) }()
	return done
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout waiting for %s", what)
	}
}

// A normal attach configures the tap and serves DNS on the gateway of the
// lowest free link.
func TestAttachConfiguresTapAndDNS(t *testing.T) {
	m, h := newFakeManager(t)
	link := mustAttach(t, m, "sandbox-a")
	if link.Index != 1 {
		t.Fatalf("index = %d, want 1", link.Index)
	}
	tap := TapName(link.Index)
	want := []string{
		"ip tuntap add dev " + tap + " mode tap",
		"ip addr add " + link.Gateway.String() + "/30 dev " + tap,
		"ip link set " + tap + " up",
	}
	h.mu.Lock()
	cmds := strings.Join(h.cmds, "\n")
	addrs := append([]netip.AddrPort(nil), h.dnsAddrs...)
	h.mu.Unlock()
	for _, w := range want {
		if !strings.Contains(cmds, w) {
			t.Errorf("missing command %q in:\n%s", w, cmds)
		}
	}
	if len(addrs) != 1 || addrs[0] != netip.AddrPortFrom(link.Gateway, 53) {
		t.Fatalf("DNS listeners = %v, want the gateway %s port 53", addrs, link.Gateway)
	}
}

// Detach of an unknown sandbox changes nothing.
func TestDetachUnknownSandboxIsNoop(t *testing.T) {
	m, h := newFakeManager(t)
	mustAttach(t, m, "sandbox-a")
	h.mu.Lock()
	before := len(h.cmds)
	h.mu.Unlock()
	m.Detach("unknown")
	h.mu.Lock()
	after := len(h.cmds)
	h.mu.Unlock()
	if before != after {
		t.Fatalf("Detach of an unknown sandbox ran %d commands", after-before)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.used[1] || len(m.attached) != 1 {
		t.Fatalf("state changed: used=%v attached=%d", m.used, len(m.attached))
	}
}

// The link stays reserved while DNS closes, and Detach does not hold the
// manager mutex during the teardown.
func TestDetachReservesLinkUntilDNSClosed(t *testing.T) {
	m, h := newFakeManager(t)
	mustAttach(t, m, "sandbox-a")
	closing, release := make(chan struct{}, 1), make(chan struct{})
	h.mu.Lock()
	h.dnsClosing, h.dnsRelease = closing, release
	h.mu.Unlock()
	done := detachAsync(m, "sandbox-a")
	waitFor(t, closing, "DNS close to start")
	// Later DNS forwarders must not block.
	h.mu.Lock()
	h.dnsClosing, h.dnsRelease = nil, nil
	h.mu.Unlock()

	// Another sandbox gets link 2 (and Attach is not blocked by the mutex).
	if link := mustAttach(t, m, "sandbox-b"); link.Index != 2 {
		t.Fatalf("second attach index = %d, want 2 while link 1 is being torn down", link.Index)
	}
	close(release)
	waitFor(t, done, "Detach")
	// After the teardown, link 1 is the lowest free index again.
	if link := mustAttach(t, m, "sandbox-c"); link.Index != 1 {
		t.Fatalf("third attach index = %d, want 1 after the teardown", link.Index)
	}
}

// The link stays reserved until the tap device is deleted.
func TestDetachReservesLinkUntilTapDeleted(t *testing.T) {
	m, h := newFakeManager(t)
	mustAttach(t, m, "sandbox-a")
	started, release := make(chan struct{}, 1), make(chan struct{})
	h.mu.Lock()
	h.delStarted, h.delRelease = started, release
	h.mu.Unlock()
	done := detachAsync(m, "sandbox-a")
	waitFor(t, started, "tap deletion to start")
	// Later deletions must not block.
	h.mu.Lock()
	h.delStarted, h.delRelease = nil, nil
	h.mu.Unlock()

	if link := mustAttach(t, m, "sandbox-b"); link.Index != 2 {
		t.Fatalf("second attach index = %d, want 2 while the tap of link 1 exists", link.Index)
	}
	close(release)
	waitFor(t, done, "Detach")
	if link := mustAttach(t, m, "sandbox-c"); link.Index != 1 {
		t.Fatalf("third attach index = %d, want 1 after the teardown", link.Index)
	}
}

// A failed deletion of a tap that still exists keeps the link reserved and
// logs the warning; a failed deletion of an absent tap releases the link.
func TestDetachFailedTapDeletion(t *testing.T) {
	m, h := newFakeManager(t)
	mustAttach(t, m, "sandbox-a")
	h.mu.Lock()
	h.delErr = errors.New("operation not permitted")
	h.mu.Unlock()
	m.Detach("sandbox-a")
	log := h.log()
	for _, want := range []string{`"msg":"delete tap device failed"`, `"sandbox_id":"sandbox-a"`, `"tap":"` + TapName(1) + `"`, `"error":`} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %s:\n%s", want, log)
		}
	}
	if link := mustAttach(t, m, "sandbox-b"); link.Index != 2 {
		t.Fatalf("index = %d, want 2: link 1 must stay reserved while its tap exists", link.Index)
	}

	// The tap vanished: the failed deletion no longer keeps the link.
	h.mu.Lock()
	delete(h.taps, TapName(2))
	h.mu.Unlock()
	m.Detach("sandbox-b")
	if link := mustAttach(t, m, "sandbox-c"); link.Index != 2 {
		t.Fatalf("index = %d, want 2: the absent tap frees link 2 (link 1 stays reserved)", link.Index)
	}
}

// A failed setup that cannot remove the tap keeps its link reserved; one that
// leaves no tap releases it.
func TestAttachFailureReservation(t *testing.T) {
	m, h := newFakeManager(t)
	h.startDNSErr = errors.New("address already in use")
	if _, _, err := m.Attach("sandbox-a"); err == nil {
		t.Fatal("Attach succeeded with a failing DNS start")
	}
	h.mu.Lock()
	h.startDNSErr = nil
	h.mu.Unlock()
	// The fake deleted the tap, so link 1 is free again.
	if link := mustAttach(t, m, "sandbox-b"); link.Index != 1 {
		t.Fatalf("index = %d, want 1 after a clean failure", link.Index)
	}

	h.mu.Lock()
	h.startDNSErr = errors.New("address already in use")
	h.delErr = errors.New("operation not permitted")
	h.mu.Unlock()
	if _, _, err := m.Attach("sandbox-c"); err == nil {
		t.Fatal("Attach succeeded with a failing DNS start")
	}
	h.mu.Lock()
	h.startDNSErr = nil
	h.delErr = nil
	h.mu.Unlock()
	if link := mustAttach(t, m, "sandbox-d"); link.Index != 3 {
		t.Fatalf("index = %d, want 3: link 2 stays reserved while its tap exists", link.Index)
	}
}

// tapDeviceExists confirms an absent device without root.
func TestTapDeviceExistsAbsent(t *testing.T) {
	if tapDeviceExists(TapName(1<<14 - 1)) {
		t.Fatal("a device that does not exist is reported as present")
	}
	if !tapDeviceExists("lo") {
		t.Skip("no loopback interface in this environment")
	}
}

// Two sandboxes whose IDs share the last 11 hex digits (the 44-bit suffix
// that the former name used) get distinct taps, links and gateways, and the
// second attach does not delete the tap of the first.
func TestAttachDistinctTapsForSameIDSuffix(t *testing.T) {
	m, h := newFakeManager(t)
	idA := "0192f4b6-7c3a-7def-8abc-0123456789ab"
	idB := "0192f4b6-7c3b-7000-8abc-1123456789ab"
	linkA, tapA, err := m.Attach(idA)
	if err != nil {
		t.Fatalf("Attach(%s): %v", idA, err)
	}
	linkB, tapB, err := m.Attach(idB)
	if err != nil {
		t.Fatalf("Attach(%s): %v", idB, err)
	}
	if tapA == tapB {
		t.Fatalf("both sandboxes share the tap %q", tapA)
	}
	if linkA.Index == linkB.Index || linkA.Gateway == linkB.Gateway || linkA.Guest == linkB.Guest {
		t.Fatalf("links overlap: %+v %+v", linkA, linkB)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.taps[tapA] || !h.taps[tapB] {
		t.Fatalf("taps = %v, want both %q and %q present", h.taps, tapA, tapB)
	}
	// Only the first attach's own pre-create cleanup may delete its tap.
	dels := 0
	for _, c := range h.cmds {
		if c == "ip link del "+tapA {
			dels++
		}
	}
	if dels != 1 {
		t.Fatalf("`ip link del %s` ran %d times, want 1 (the second attach must not delete it): %v", tapA, dels, h.cmds)
	}
}

// While the teardown of a sandbox is pending, a new attach for an ID with
// the same suffix gets another tap name and leaves the pending tap alone.
func TestAttachDuringPendingDetachKeepsDistinctTap(t *testing.T) {
	m, h := newFakeManager(t)
	idA := "0192f4b6-7c3a-7def-8abc-0123456789ab"
	idB := "0192f4b6-7c3b-7000-8abc-1123456789ab"
	_, tapA, err := m.Attach(idA)
	if err != nil {
		t.Fatalf("Attach(%s): %v", idA, err)
	}
	release := make(chan struct{})
	closing := make(chan struct{}, 1)
	h.mu.Lock()
	h.dnsRelease = release
	h.dnsClosing = closing
	h.mu.Unlock()
	done := detachAsync(m, idA)
	waitFor(t, closing, "DNS close start")
	_, tapB, err := m.Attach(idB)
	if err != nil {
		t.Fatalf("Attach(%s): %v", idB, err)
	}
	if tapB == tapA {
		t.Fatalf("pending and new attachment share the tap %q", tapA)
	}
	h.mu.Lock()
	present := h.taps[tapA]
	h.mu.Unlock()
	if !present {
		t.Fatalf("the pending tap %q was deleted by the new attach", tapA)
	}
	close(release)
	waitFor(t, done, "Detach")
}

// useRealRefresh makes the manager render the real ruleset from its DNS
// assignments with a fixed host routing table; every applied script is
// recorded and applyErr (when set) fails the application.
func useRealRefresh(m *Manager) (applied func() []string, setApplyErr func(error)) {
	var mu sync.Mutex
	var scripts []string
	var applyErr error
	m.hostRoutes = func() ([]Route, []netip.Prefix, error) { return nil, nil, nil }
	m.applyRules = func(rules string) error {
		mu.Lock()
		defer mu.Unlock()
		if applyErr != nil {
			return applyErr
		}
		scripts = append(scripts, rules)
		return nil
	}
	m.refresh = m.Refresh
	return func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), scripts...)
		}, func(err error) {
			mu.Lock()
			defer mu.Unlock()
			applyErr = err
		}
}

func dnsRuleLines(rules string) []string {
	var out []string
	for _, l := range strings.Split(rules, "\n") {
		if l = strings.TrimSpace(l); strings.Contains(l, "dport 53 accept") {
			out = append(out, l)
		}
	}
	return out
}

func dnsRulesFor(l Link) []string {
	tap := TapName(l.Index)
	return []string{
		`iifname "` + tap + `" ip daddr ` + l.Gateway.String() + ` udp dport 53 accept`,
		`iifname "` + tap + `" ip daddr ` + l.Gateway.String() + ` tcp dport 53 accept`,
	}
}

// Attach and Detach change only the DNS rules of their own tap, and a
// detached tap leaves no exception behind.
func TestAttachDetachChangeOnlyOwnDNSRules(t *testing.T) {
	m, _ := newFakeManager(t)
	applied, _ := useRealRefresh(m)
	a := mustAttach(t, m, "sandbox-a")
	last := func() string {
		s := applied()
		if len(s) == 0 {
			t.Fatal("no ruleset applied")
		}
		return s[len(s)-1]
	}
	if got, want := dnsRuleLines(last()), dnsRulesFor(a); !slices.Equal(got, want) {
		t.Fatalf("after attach a: %q, want %q", got, want)
	}
	b := mustAttach(t, m, "sandbox-b")
	if got, want := dnsRuleLines(last()), append(dnsRulesFor(a), dnsRulesFor(b)...); !slices.Equal(got, want) {
		t.Fatalf("after attach b: %q, want %q", got, want)
	}
	m.Detach("sandbox-a")
	if got, want := dnsRuleLines(last()), dnsRulesFor(b); !slices.Equal(got, want) {
		t.Fatalf("after detach a: %q, want only the rules of b %q", got, want)
	}
	m.Detach("sandbox-b")
	if got := dnsRuleLines(last()); len(got) != 0 {
		t.Fatalf("after detach b: DNS rules remain: %q", got)
	}
	// A reused tap name gets exactly its own gateway.
	c := mustAttach(t, m, "sandbox-c")
	if c.Index != a.Index {
		t.Fatalf("index = %d, want the reused %d", c.Index, a.Index)
	}
	if got, want := dnsRuleLines(last()), dnsRulesFor(c); !slices.Equal(got, want) {
		t.Fatalf("after reuse: %q, want %q", got, want)
	}
}

// A failed attach leaves no DNS exception: not when the policy refresh
// fails, and not when the DNS forwarder fails after the refresh applied it.
func TestFailedAttachLeavesNoDNSException(t *testing.T) {
	m, h := newFakeManager(t)
	applied, setApplyErr := useRealRefresh(m)
	staged := func() int {
		m.mu.Lock()
		defer m.mu.Unlock()
		return len(m.gateways)
	}

	setApplyErr(errors.New("nft failed"))
	if _, _, err := m.Attach("sandbox-a"); err == nil {
		t.Fatal("Attach succeeded with a failing ruleset application")
	}
	if n := staged(); n != 0 {
		t.Fatalf("%d staged assignments after a failed refresh", n)
	}
	setApplyErr(nil)

	h.mu.Lock()
	h.startDNSErr = errors.New("address already in use")
	h.mu.Unlock()
	if _, _, err := m.Attach("sandbox-b"); err == nil {
		t.Fatal("Attach succeeded with a failing DNS start")
	}
	h.mu.Lock()
	h.startDNSErr = nil
	h.mu.Unlock()
	scripts := applied()
	if len(scripts) < 2 {
		t.Fatalf("want a ruleset with the exception and one without, got %d scripts", len(scripts))
	}
	if got := dnsRuleLines(scripts[0]); len(got) != 2 {
		t.Fatalf("first applied ruleset DNS rules = %q, want the two rules of the attaching tap", got)
	}
	if got := dnsRuleLines(scripts[len(scripts)-1]); len(got) != 0 {
		t.Fatalf("the ruleset after the failed attach keeps DNS rules: %q", got)
	}
	if n := staged(); n != 0 {
		t.Fatalf("%d staged assignments after a failed attach", n)
	}
}
