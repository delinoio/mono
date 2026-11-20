package datapath

import (
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	sdkdp "github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"google.golang.org/protobuf/types/known/durationpb"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type fakeStream struct {
	mu     sync.Mutex
	reason ownsandv1.SessionEndReason
}

func (f *fakeStream) endWith(r ownsandv1.SessionEndReason) {
	f.mu.Lock()
	f.reason = r
	f.mu.Unlock()
}

func (f *fakeStream) ended() ownsandv1.SessionEndReason {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reason
}

type envLog struct {
	mu   sync.Mutex
	envs []*ownsandv1.AgentEnvelope
}

func (e *envLog) send(env *ownsandv1.AgentEnvelope) bool {
	e.mu.Lock()
	e.envs = append(e.envs, env)
	e.mu.Unlock()
	return true
}

func (e *envLog) releases() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, env := range e.envs {
		if r := env.GetRelayGrantRelease(); r != nil {
			out = append(out, r.GetGrantId())
		}
	}
	return out
}

func newTable() (*grantTable, *fakeClock, *envLog) {
	c := &fakeClock{now: time.Unix(1_000_000, 0)}
	t := newGrantTable(quiet, c.Now)
	l := &envLog{}
	t.setChannel(1, l.send)
	return t, c, l
}

func relayGrant(sandboxID string) *ownsandv1.RelayGrant {
	return &ownsandv1.RelayGrant{
		GrantId: sdkdp.NewID(), SandboxId: sandboxID, OperationKind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC,
		OperationId: sdkdp.NewID(), ExpiresIn: durationpb.New(15 * time.Minute), StreamDeadlineIn: durationpb.New(30 * time.Minute),
	}
}

func TestGrantLimitAndReplacement(t *testing.T) {
	tb, _, _ := newTable()
	var first *ownsandv1.RelayGrant
	for i := 0; i < MaxGrants; i++ {
		g := relayGrant("sb")
		if i == 0 {
			first = g
		}
		if ack := tb.install(1, g); ack.GetResult() != ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_ACCEPTED {
			t.Fatalf("grant %d rejected", i)
		}
	}
	if ack := tb.install(1, relayGrant("sb")); ack.GetResult() != ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_REJECTED_GRANT_LIMIT {
		t.Fatal("grant above the limit accepted")
	}
	// A replacement takes the old grant's slot at the limit and ends its
	// open streams with GRANT_REPLACED.
	s := &fakeStream{}
	if _, err := tb.openStream(first.GetGrantId(), "sb", s, 8); err != nil {
		t.Fatal(err)
	}
	rep := relayGrant("sb")
	old := first.GetGrantId()
	rep.ReplacesGrantId = &old
	ack := tb.install(1, rep)
	if ack.GetResult() != ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_ACCEPTED || ack.GetReplacedGrantId() != old {
		t.Fatalf("replacement ack %v", ack)
	}
	if s.ended() != ownsandv1.SessionEndReason_SESSION_END_REASON_GRANT_REPLACED || tb.count() != MaxGrants {
		t.Fatalf("replaced stream %v, count %d", s.ended(), tb.count())
	}
	// A replaces_grant_id for no held grant counts as a new grant.
	stale := relayGrant("sb")
	missing := sdkdp.NewID()
	stale.ReplacesGrantId = &missing
	if ack := tb.install(1, stale); ack.GetResult() != ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_REJECTED_GRANT_LIMIT || ack.ReplacedGrantId != nil {
		t.Fatalf("stale replacement %v", ack)
	}
}

func TestGrantExpiryToleranceAndStreamDeadline(t *testing.T) {
	tb, clock, log := newTable()
	g := relayGrant("sb")
	tb.install(1, g)
	if _, err := tb.openStream(g.GetGrantId(), "other", &fakeStream{}, 8); err != errGrantInvalid {
		t.Fatal("a stream for another sandbox was admitted")
	}
	clock.Advance(15*time.Minute + 29*time.Second)
	s := &fakeStream{}
	gr, err := tb.openStream(g.GetGrantId(), "sb", s, 8)
	if err != nil || !tb.admitRequest(gr) {
		t.Fatal("a stream within the admission tolerance was rejected")
	}
	clock.Advance(2 * time.Second)
	if _, err := tb.openStream(g.GetGrantId(), "sb", &fakeStream{}, 8); err != errGrantInvalid {
		t.Fatal("a stream after the tolerance was admitted")
	}
	if tb.hasLive() {
		t.Fatal("an expired grant counts as live")
	}
	// The admitted stream continues until the stream deadline.
	tb.sweep()
	if s.ended() != 0 || tb.count() != 1 {
		t.Fatal("the open stream ended at the expiry")
	}
	clock.Advance(15 * time.Minute)
	tb.sweep()
	if s.ended() != ownsandv1.SessionEndReason_SESSION_END_REASON_EXPIRED || tb.count() != 0 {
		t.Fatalf("stream deadline: %v, count %d", s.ended(), tb.count())
	}
	if len(log.releases()) != 1 {
		t.Fatal("no release at the stream deadline")
	}
}

func TestGrantReleaseRules(t *testing.T) {
	tb, clock, log := newTable()
	// Idle release after 60 seconds without a stream.
	idle := relayGrant("sb")
	tb.install(1, idle)
	clock.Advance(59 * time.Second)
	tb.sweep()
	if tb.count() != 1 {
		t.Fatal("released before 60 seconds")
	}
	clock.Advance(2 * time.Second)
	tb.sweep()
	if tb.count() != 0 {
		t.Fatal("idle grant not released")
	}
	// Release when the operation finished and no stream is open.
	done := relayGrant("sb")
	tb.install(1, done)
	s := &fakeStream{}
	gr, _ := tb.openStream(done.GetGrantId(), "sb", s, 8)
	tb.streamEnded(gr, s, Outcome{Finished: true, Started: true}, false)
	if tb.count() != 0 {
		t.Fatal("finished grant not released")
	}
	// An error that leaves no operation releases; AttachExec not_found under
	// an EXEC grant does not.
	keep := relayGrant("sb")
	tb.install(1, keep)
	gr, _ = tb.openStream(keep.GetGrantId(), "sb", s, 8)
	tb.streamEnded(gr, s, Outcome{Code: connect.CodeNotFound}, true)
	if tb.count() != 1 {
		t.Fatal("AttachExec not_found under an EXEC grant released it")
	}
	gr, _ = tb.openStream(keep.GetGrantId(), "sb", s, 8)
	tb.streamEnded(gr, s, Outcome{Code: connect.CodePermissionDenied}, false)
	if tb.count() != 0 {
		t.Fatal("permission_denied did not release")
	}
	if len(log.releases()) != 3 {
		t.Fatalf("releases %v", log.releases())
	}
	// An expired grant is released as soon as its last stream ends.
	exp := relayGrant("sb")
	tb.install(1, exp)
	gr, _ = tb.openStream(exp.GetGrantId(), "sb", s, 8)
	clock.Advance(16 * time.Minute)
	tb.streamEnded(gr, s, Outcome{Started: true}, false)
	if tb.count() != 0 {
		t.Fatal("expired grant kept after its last stream")
	}
}

func TestGrantRevocationAndChannelLoss(t *testing.T) {
	tb, _, _ := newTable()
	a, b := relayGrant("sb"), relayGrant("sb")
	tb.install(1, a)
	tb.install(1, b)
	sa, sb := &fakeStream{}, &fakeStream{}
	_, _ = tb.openStream(a.GetGrantId(), "sb", sa, 8)
	_, _ = tb.openStream(b.GetGrantId(), "sb", sb, 8)
	tb.revoke(a.GetGrantId())
	if sa.ended() != ownsandv1.SessionEndReason_SESSION_END_REASON_REVOKED || sb.ended() != 0 {
		t.Fatal("revocation ended the wrong streams")
	}
	if _, err := tb.openStream(a.GetGrantId(), "sb", &fakeStream{}, 8); err != errGrantInvalid {
		t.Fatal("a revoked grant admitted a stream")
	}
	tb.channelLost(1)
	if sb.ended() != ownsandv1.SessionEndReason_SESSION_END_REASON_AGENT_CHANNEL_LOST || tb.count() != 0 {
		t.Fatal("channel loss did not end the grant")
	}
	// A grant that arrived on an older channel is not installed.
	tb.setChannel(2, nil)
	if ack := tb.install(1, relayGrant("sb")); ack.GetResult() != ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_UNSPECIFIED {
		t.Fatal("grant of an old channel installed")
	}
}

func TestPerGrantStreamCap(t *testing.T) {
	tb, _, _ := newTable()
	g := relayGrant("sb")
	tb.install(1, g)
	for i := 0; i < 2; i++ {
		if _, err := tb.openStream(g.GetGrantId(), "sb", &fakeStream{}, 2); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tb.openStream(g.GetGrantId(), "sb", &fakeStream{}, 2); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("third stream: %v", err)
	}
}
