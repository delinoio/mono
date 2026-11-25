// SPDX-License-Identifier: FSL-1.1-ALv2

package netpolicy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
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
	// Stop the refresh worker even when an assertion below fails first.
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after the test ended")
		}
	})

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
	m.listTaps = func() ([]string, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		taps := make([]string, 0, len(h.taps))
		for tap := range h.taps {
			taps = append(taps, tap)
		}
		slices.Sort(taps)
		return taps, nil
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

// attachResult is the outcome of an Attach call made off the test goroutine.
type attachResult struct {
	link Link
	tap  string
	err  error
}

// attachAsync runs Attach in a goroutine, so that a competing operation that
// blocks on the manager lock fails the test within a bound instead of hanging
// it. The goroutine must not call t.Fatal, so it reports through the channel.
func attachAsync(m *Manager, id string) <-chan attachResult {
	res := make(chan attachResult, 1)
	go func() {
		link, tap, err := m.Attach(id)
		res <- attachResult{link: link, tap: tap, err: err}
	}()
	return res
}

// awaitAttach waits for an Attach started by attachAsync within the test bound
// and checks its tap name, as mustAttach does.
func awaitAttach(t *testing.T, res <-chan attachResult, id, what string) Link {
	t.Helper()
	select {
	case r := <-res:
		if r.err != nil {
			t.Fatalf("Attach(%s): %v", id, r.err)
		}
		if r.tap != TapName(r.link.Index) {
			t.Fatalf("Attach(%s) tap = %q, want %q", id, r.tap, TapName(r.link.Index))
		}
		return r.link
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout waiting for %s", what)
		return Link{}
	}
}

// onceFunc returns a function that closes ch the first time it is called.
func onceFunc(ch chan struct{}) func() {
	var once sync.Once
	return func() { once.Do(func() { close(ch) }) }
}

// releaseOnCleanup opens a held fixture gate before waiting for the worker
// it blocks, even if a fatal assertion ends the test early.
func releaseOnCleanup(t *testing.T, open func(), done <-chan struct{}, worker string) {
	t.Helper()
	t.Cleanup(func() {
		open()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("%s did not return after the held fixture was released", worker)
		}
	})
}

// startGatedWorker registers cleanup before starting the worker. Closing done
// publishes its result and lets both the test and cleanup observe completion.
func startGatedWorker(t *testing.T, open func(), worker string, run func() error) func() error {
	t.Helper()
	done := make(chan struct{})
	var err error
	releaseOnCleanup(t, open, done, worker)
	go func() {
		err = run()
		close(done)
	}()
	return func() error {
		t.Helper()
		waitFor(t, done, worker)
		return err
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
	openRelease := onceFunc(release)
	h.mu.Lock()
	h.dnsClosing, h.dnsRelease = closing, release
	h.mu.Unlock()
	done := detachAsync(m, "sandbox-a")
	releaseOnCleanup(t, openRelease, done, "Detach")
	waitFor(t, closing, "DNS close to start")
	// Later DNS forwarders must not block.
	h.mu.Lock()
	h.dnsClosing, h.dnsRelease = nil, nil
	h.mu.Unlock()

	// Another sandbox gets link 2 (and Attach is not blocked by the mutex).
	if link := awaitAttach(t, attachAsync(m, "sandbox-b"), "sandbox-b", "Attach while DNS closes"); link.Index != 2 {
		t.Fatalf("second attach index = %d, want 2 while link 1 is being torn down", link.Index)
	}
	openRelease()
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
	openRelease := onceFunc(release)
	h.mu.Lock()
	h.delStarted, h.delRelease = started, release
	h.mu.Unlock()
	done := detachAsync(m, "sandbox-a")
	releaseOnCleanup(t, openRelease, done, "Detach")
	waitFor(t, started, "tap deletion to start")
	// Later deletions must not block.
	h.mu.Lock()
	h.delStarted, h.delRelease = nil, nil
	h.mu.Unlock()

	if link := awaitAttach(t, attachAsync(m, "sandbox-b"), "sandbox-b", "Attach while the tap deletion runs"); link.Index != 2 {
		t.Fatalf("second attach index = %d, want 2 while the tap of link 1 exists", link.Index)
	}
	openRelease()
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
	openRelease := onceFunc(release)
	h.mu.Lock()
	h.dnsRelease = release
	h.dnsClosing = closing
	h.mu.Unlock()
	done := detachAsync(m, idA)
	releaseOnCleanup(t, openRelease, done, "Detach")
	waitFor(t, closing, "DNS close start")
	// Later DNS forwarders must not block.
	h.mu.Lock()
	h.dnsRelease, h.dnsClosing = nil, nil
	h.mu.Unlock()
	linkB := awaitAttach(t, attachAsync(m, idB), idB, "Attach while the detach is pending")
	tapB := TapName(linkB.Index)
	if tapB == tapA {
		t.Fatalf("pending and new attachment share the tap %q", tapA)
	}
	h.mu.Lock()
	present := h.taps[tapA]
	h.mu.Unlock()
	if !present {
		t.Fatalf("the pending tap %q was deleted by the new attach", tapA)
	}
	openRelease()
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
		`iifname "` + tap + `" ip saddr ` + l.Guest.String() + ` ip daddr ` + l.Gateway.String() + ` udp dport 53 accept`,
		`iifname "` + tap + `" ip saddr ` + l.Guest.String() + ` ip daddr ` + l.Gateway.String() + ` tcp dport 53 accept`,
	}
}

// sourceGuardLinesFor returns the forward chain rule that drops packets
// from the tap of l whose source is not its guest address.
func sourceGuardLinesFor(l Link) []string {
	return []string{`iifname "` + TapName(l.Index) + `" ip saddr != ` + l.Guest.String() + ` drop`}
}

// sourceGuardLines returns the per-tap source guard lines of a ruleset.
func sourceGuardLines(rules string) []string {
	var out []string
	for _, l := range strings.Split(rules, "\n") {
		if l = strings.TrimSpace(l); strings.Contains(l, " ip saddr != ") {
			out = append(out, l)
		}
	}
	return out
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
	if got, want := sourceGuardLines(last()), sourceGuardLinesFor(a); !slices.Equal(got, want) {
		t.Fatalf("after attach a: source guards %q, want %q", got, want)
	}
	b := mustAttach(t, m, "sandbox-b")
	if got, want := dnsRuleLines(last()), append(dnsRulesFor(a), dnsRulesFor(b)...); !slices.Equal(got, want) {
		t.Fatalf("after attach b: %q, want %q", got, want)
	}
	if got, want := sourceGuardLines(last()), append(sourceGuardLinesFor(a), sourceGuardLinesFor(b)...); !slices.Equal(got, want) {
		t.Fatalf("after attach b: source guards %q, want %q", got, want)
	}
	m.Detach("sandbox-a")
	if got, want := dnsRuleLines(last()), dnsRulesFor(b); !slices.Equal(got, want) {
		t.Fatalf("after detach a: %q, want only the rules of b %q", got, want)
	}
	if got, want := sourceGuardLines(last()), sourceGuardLinesFor(b); !slices.Equal(got, want) {
		t.Fatalf("after detach a: source guards %q, want only the guard of b %q", got, want)
	}
	m.Detach("sandbox-b")
	if got := dnsRuleLines(last()); len(got) != 0 {
		t.Fatalf("after detach b: DNS rules remain: %q", got)
	}
	if got := sourceGuardLines(last()); len(got) != 0 {
		t.Fatalf("after detach b: source guards remain: %q", got)
	}
	// A reused tap name gets exactly its own gateway.
	c := mustAttach(t, m, "sandbox-c")
	if c.Index != a.Index {
		t.Fatalf("index = %d, want the reused %d", c.Index, a.Index)
	}
	if got, want := dnsRuleLines(last()), dnsRulesFor(c); !slices.Equal(got, want) {
		t.Fatalf("after reuse: %q, want %q", got, want)
	}
	if got, want := sourceGuardLines(last()), sourceGuardLinesFor(c); !slices.Equal(got, want) {
		t.Fatalf("after reuse: source guards %q, want %q", got, want)
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

// An attach whose own refresh fails after an independent refresh (the
// periodic poll or another attach) applied its staged DNS assignment still
// removes the DNS rules of its tap before it returns, and keeps the rules of
// the other taps.
func TestFailedAttachRemovesDNSRulesAppliedByIndependentRefresh(t *testing.T) {
	m, _ := newFakeManager(t)
	applied, _ := useRealRefresh(m)
	seeded, _, err := m.Attach("sandbox-a")
	if err != nil {
		t.Fatal(err)
	}

	var (
		mu          sync.Mutex
		routeReads  int
		wantRoutes  = errors.New("route read failed")
		routesRead  = make(chan struct{})
		releaseRead = make(chan struct{})
		attachReady = make(chan struct{})
	)
	openRelease := onceFunc(releaseRead)
	m.hostRoutes = func() ([]Route, []netip.Prefix, error) {
		mu.Lock()
		routeReads++
		n := routeReads
		mu.Unlock()
		switch n {
		case 1:
			// The independent refresh holds the refresh lock and has read
			// its routes. It reads the assignments only after the attach
			// staged its own.
			close(routesRead)
			<-releaseRead
			return nil, nil, nil
		case 2:
			// The refresh of the failing attach.
			return nil, nil, wantRoutes
		default:
			return nil, nil, nil
		}
	}
	refresh := m.refresh
	m.refresh = func() error {
		select {
		case <-attachReady:
		default:
			// The attach staged its assignment and starts its refresh,
			// which waits for the independent refresh.
			close(attachReady)
		}
		return refresh()
	}

	waitIndependent := startGatedWorker(t, openRelease, "independent refresh", m.Refresh)
	waitFor(t, routesRead, "the independent refresh to read the routes")

	waitAttach := startGatedWorker(t, openRelease, "Attach(sandbox-b)", func() error {
		_, _, err := m.Attach("sandbox-b")
		return err
	})
	waitFor(t, attachReady, "the attach to stage its assignment")
	openRelease()

	if err := waitIndependent(); err != nil {
		t.Fatalf("independent refresh: %v", err)
	}
	if err := waitAttach(); !errors.Is(err, wantRoutes) {
		t.Fatalf("Attach error = %v, want the route read error", err)
	}

	scripts := applied()
	if len(scripts) != 3 {
		t.Fatalf("want 3 applied rulesets (seed, independent refresh, rollback), got %d", len(scripts))
	}
	if got := dnsRuleLines(scripts[1]); len(got) != 4 {
		t.Fatalf("independent refresh applied DNS rules %q, want the rules of both taps", got)
	}
	if got, want := dnsRuleLines(scripts[2]), dnsRulesFor(seeded); !slices.Equal(got, want) {
		t.Fatalf("DNS rules after the failed attach = %q, want only those of the seeded tap %q", got, want)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.gateways) != 1 || m.gateways[TapName(seeded.Index)].Tap == "" {
		t.Fatalf("staged assignments after the failed attach = %v, want only the seeded tap", m.gateways)
	}
}

// useIPRouteDump replaces the `ip` route dump for the test.
func useIPRouteDump(t *testing.T, dump func() ([]byte, error)) {
	t.Helper()
	old := ipRouteDump
	ipRouteDump = dump
	t.Cleanup(func() { ipRouteDump = old })
}

// HostRoutes collects the routes of every IPv4 table, and the block set
// holds the prefix of a secondary table with its IPv4-mapped and NAT64 forms.
func TestHostRoutesCollectsSecondaryTables(t *testing.T) {
	useIPRouteDump(t, func() ([]byte, error) { return []byte(ipRouteShowFixture), nil })
	routes, onLink, err := HostRoutes()
	if err != nil {
		t.Fatal(err)
	}
	want := Route{Prefix: pfx("198.51.100.0/24"), Interface: "wg0", Table: "100"}
	if !slices.Contains(routes, want) {
		t.Fatalf("routes lack %v: %v", want, routes)
	}
	v4, v6 := Blocked(routes, onLink)
	if !has(v4, "198.51.100.0/24") || !has(v6, "::ffff:198.51.100.0/120") || !has(v6, "64:ff9b::c633:6400/120") {
		t.Fatal("block set lacks the prefix of the secondary table or its mapped and NAT64 forms")
	}
}

// A failing route dump is an error of HostRoutes, not an empty route list.
func TestHostRoutesDumpFailure(t *testing.T) {
	useIPRouteDump(t, func() ([]byte, error) { return nil, errors.New("ip failed") })
	if routes, _, err := HostRoutes(); err == nil || routes != nil {
		t.Fatalf("HostRoutes = %v, %v: want an error", routes, err)
	}
}

// The real route dump fails when the `ip` command exits with an error
// (an `ip` shim on PATH stands in for a host whose route dump fails).
func TestIPRouteDumpCommandFailure(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "ip")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\necho 'RTNETLINK answers: Operation not supported' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	_, err := ipRouteDump()
	if err == nil || !strings.Contains(err.Error(), "Operation not supported") {
		t.Fatalf("ipRouteDump error = %v, want the exit error with the stderr text", err)
	}
	if _, _, err := HostRoutes(); err == nil {
		t.Fatal("HostRoutes succeeded with a failing ip command")
	}
}

// Refresh returns the route error and keeps the last applied rules.
func TestRefreshKeepsRulesWhenRouteDumpFails(t *testing.T) {
	m, h := newFakeManager(t)
	applied, _ := useRealRefresh(m)
	if err := m.Refresh(); err != nil {
		t.Fatal(err)
	}
	if len(applied()) != 1 {
		t.Fatalf("%d scripts after the first refresh", len(applied()))
	}
	before := m.rules
	useIPRouteDump(t, func() ([]byte, error) { return nil, errors.New("ip failed") })
	m.hostRoutes = HostRoutes
	if err := m.Refresh(); err == nil || !strings.Contains(err.Error(), "ip failed") {
		t.Fatalf("Refresh error = %v, want the route dump error", err)
	}
	if len(applied()) != 1 || m.rules != before {
		t.Fatal("a failed route dump changed the applied rules")
	}
	// The periodic refresh logs the failure.
	m.interval = time.Millisecond
	m.refresh = m.Refresh
	m.mu.Lock()
	m.attached["s"] = &attachment{}
	m.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	// Stop the worker before useIPRouteDump restores the shared hook.
	t.Cleanup(func() {
		cancel()
		<-done
	})
	go func() { m.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(h.log(), "network policy refresh failed") {
		if time.Now().After(deadline) {
			t.Fatal("no error log for the failed refresh")
		}
		time.Sleep(time.Millisecond)
	}
}

// fakeCleanupHost drives cleanupHost without root: taps lists the sandbox
// tap devices, failures maps a command line to its error, and absent names
// the taps that no longer exist after a failed deletion.
type fakeCleanupHost struct {
	taps     []string
	listErr  error
	failures map[string]error
	absent   map[string]bool
	cmds     []string
}

func (f *fakeCleanupHost) ops() hostCleanupOps {
	return hostCleanupOps{
		listTaps: func() ([]string, error) { return f.taps, f.listErr },
		run: func(name string, args ...string) error {
			line := name + " " + strings.Join(args, " ")
			f.cmds = append(f.cmds, line)
			return f.failures[line]
		},
		tapExists: func(tap string) bool { return !f.absent[tap] },
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// Manager.CleanupHost refuses while a sandbox is attached, deletes the tap
// devices and the table through the manager's host operations once no
// sandbox is attached (the device removal), and forgets the applied
// ruleset, so that the next Attach applies the table again.
func TestManagerCleanupHost(t *testing.T) {
	m, h := newFakeManager(t)
	applied, _ := useRealRefresh(m)
	mustAttach(t, m, "sandbox-a")
	if n := len(applied()); n != 1 {
		t.Fatalf("rulesets applied by the attach = %d, want 1", n)
	}
	h.mu.Lock()
	before := len(h.cmds)
	h.mu.Unlock()
	if err := m.CleanupHost(); !errors.Is(err, errAttached) {
		t.Fatalf("CleanupHost with an attached sandbox = %v, want errAttached", err)
	}
	h.mu.Lock()
	if len(h.cmds) != before {
		t.Fatalf("CleanupHost with an attached sandbox ran %q", h.cmds[before:])
	}
	// A stale tap of an earlier process that the manager does not track.
	h.taps["osb-00000000009"] = true
	h.mu.Unlock()
	m.Detach("sandbox-a")
	h.mu.Lock()
	before = len(h.cmds)
	h.mu.Unlock()
	if err := m.CleanupHost(); err != nil {
		t.Fatalf("CleanupHost: %v", err)
	}
	h.mu.Lock()
	got := append([]string(nil), h.cmds[before:]...)
	left := len(h.taps)
	h.mu.Unlock()
	want := []string{"ip link del osb-00000000009", "nft delete table inet ownsand"}
	if !slices.Equal(got, want) {
		t.Fatalf("CleanupHost commands %q, want %q", got, want)
	}
	if left != 0 {
		t.Fatalf("%d tap devices left after CleanupHost", left)
	}
	// The table is gone, so the next attach applies the same ruleset again.
	n := len(applied())
	mustAttach(t, m, "sandbox-b")
	if got := len(applied()); got != n+1 {
		t.Fatalf("rulesets applied by the attach after CleanupHost = %d, want 1", got-n)
	}
}

// A periodic refresh that reads the host routes while the device removal
// runs CleanupHost applies nothing, so the table that the clean-up deleted
// stays deleted.
func TestManagerRefreshInFlightDuringCleanupHost(t *testing.T) {
	m, _ := newFakeManager(t)
	applied, _ := useRealRefresh(m)
	mustAttach(t, m, "sandbox-a")
	m.Detach("sandbox-a")
	entered := make(chan struct{})
	release := make(chan struct{})
	openRelease := onceFunc(release)
	m.hostRoutes = func() ([]Route, []netip.Prefix, error) {
		close(entered)
		<-release
		return nil, nil, nil
	}
	waitRefresh := startGatedWorker(t, openRelease, "Refresh during CleanupHost", m.Refresh)
	waitFor(t, entered, "the route read before CleanupHost")
	if err := m.CleanupHost(); err != nil {
		t.Fatalf("CleanupHost: %v", err)
	}
	n := len(applied())
	openRelease()
	if err := waitRefresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := len(applied()); got != n {
		t.Fatalf("the refresh in flight applied %d rulesets after CleanupHost, want 0", got-n)
	}
	m.mu.Lock()
	rules := m.rules
	m.mu.Unlock()
	if rules != "" {
		t.Fatal("the refresh in flight recorded a ruleset after CleanupHost")
	}
}

// Two concurrent refreshes run one after the other: the second does not
// read the routes before the first finished, so the older route snapshot
// cannot be applied after the newer one.
func TestManagerRefreshSerializesRouteSnapshots(t *testing.T) {
	m, _ := newFakeManager(t)
	applied, _ := useRealRefresh(m)
	first := []netip.Prefix{netip.MustParsePrefix("10.1.0.0/16")}
	second := []netip.Prefix{netip.MustParsePrefix("10.2.0.0/16")}
	var reads atomic.Int32
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondEntered := make(chan struct{})
	openRelease := onceFunc(releaseFirst)
	m.hostRoutes = func() ([]Route, []netip.Prefix, error) {
		switch reads.Add(1) {
		case 1:
			close(firstEntered)
			<-releaseFirst
			return nil, first, nil
		default:
			close(secondEntered)
			return nil, second, nil
		}
	}
	waitFirst := startGatedWorker(t, openRelease, "first Refresh", m.Refresh)
	waitFor(t, firstEntered, "the first route read")
	waitSecond := startGatedWorker(t, openRelease, "second Refresh", m.Refresh)
	select {
	case <-secondEntered:
		t.Fatal("the second refresh read the routes while the first was in flight")
	case <-time.After(100 * time.Millisecond):
	}
	openRelease()
	if err := waitFirst(); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}
	if err := waitSecond(); err != nil {
		t.Fatalf("second Refresh: %v", err)
	}
	v4, v6 := Blocked(nil, second)
	want := Ruleset(v4, v6, nil)
	m.mu.Lock()
	got := m.rules
	m.mu.Unlock()
	if got != want {
		t.Fatalf("cached rules are not the ruleset of the second route snapshot:\n%s", got)
	}
	scripts := applied()
	if len(scripts) != 2 || scripts[len(scripts)-1] != want {
		t.Fatalf("applied %d scripts, the last one is not the second snapshot", len(scripts))
	}
}

// Every tap and the table are deleted, and the clean-up reports no error.
func TestCleanupHostDeletesEverything(t *testing.T) {
	f := &fakeCleanupHost{taps: []string{"osb-00000000001", "osb-00000000002"}}
	if err := cleanupHost(discardLogger(), f.ops()); err != nil {
		t.Fatalf("cleanupHost: %v", err)
	}
	want := []string{"ip link del osb-00000000001", "ip link del osb-00000000002", "nft delete table inet ownsand"}
	if !slices.Equal(f.cmds, want) {
		t.Fatalf("commands %q, want %q", f.cmds, want)
	}
}

// A failed tap deletion does not stop the other taps or the table; the error
// names each failed resource.
func TestCleanupHostReportsFailuresAndAttemptsEverything(t *testing.T) {
	f := &fakeCleanupHost{
		taps: []string{"osb-00000000001", "osb-00000000002"},
		failures: map[string]error{
			"ip link del osb-00000000001":   errors.New("operation not permitted"),
			"nft delete table inet ownsand": errors.New("nft: operation not permitted"),
		},
	}
	err := cleanupHost(discardLogger(), f.ops())
	if err == nil {
		t.Fatal("cleanupHost reported no error")
	}
	for _, want := range []string{"delete tap device osb-00000000001", "delete nftables table inet ownsand"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "osb-00000000002") {
		t.Fatalf("error %q names a tap that was deleted", err)
	}
	if len(f.cmds) != 3 {
		t.Fatalf("commands %q: every resource must be attempted", f.cmds)
	}
}

// A resource that is already absent counts as cleaned: a tap that vanished
// during the deletion, and a table that nft reports as missing.
func TestCleanupHostAbsentResourcesAreClean(t *testing.T) {
	for _, tableErr := range []error{
		errors.New("netpolicy: nft delete table inet ownsand: exit status 1: Error: Could not process rule: No such file or directory"),
		fmt.Errorf("netpolicy: nft: %w", exec.ErrNotFound),
		errors.New("table does not exist"),
	} {
		f := &fakeCleanupHost{
			taps: []string{"osb-00000000001"},
			failures: map[string]error{
				"ip link del osb-00000000001":   errors.New("Cannot find device"),
				"nft delete table inet ownsand": tableErr,
			},
			absent: map[string]bool{"osb-00000000001": true},
		}
		if err := cleanupHost(discardLogger(), f.ops()); err != nil {
			t.Fatalf("table error %q: cleanupHost: %v", tableErr, err)
		}
	}
}

// A failed interface listing is reported, and the table is still deleted.
func TestCleanupHostListFailure(t *testing.T) {
	f := &fakeCleanupHost{listErr: errors.New("netlink failed")}
	err := cleanupHost(discardLogger(), f.ops())
	if err == nil || !strings.Contains(err.Error(), "list tap devices") {
		t.Fatalf("error %v", err)
	}
	if !slices.Equal(f.cmds, []string{"nft delete table inet ownsand"}) {
		t.Fatalf("commands %q", f.cmds)
	}
}

// A retry after a partial failure succeeds once the remaining resource is
// removable.
func TestCleanupHostRetryAfterPartialFailure(t *testing.T) {
	f := &fakeCleanupHost{
		taps:     []string{"osb-00000000001"},
		failures: map[string]error{"ip link del osb-00000000001": errors.New("operation not permitted")},
	}
	if err := cleanupHost(discardLogger(), f.ops()); err == nil {
		t.Fatal("first attempt reported no error")
	}
	f.failures = nil
	f.cmds = nil
	if err := cleanupHost(discardLogger(), f.ops()); err != nil {
		t.Fatalf("retry: %v", err)
	}
}
