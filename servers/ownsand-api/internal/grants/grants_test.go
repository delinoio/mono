// SPDX-License-Identifier: FSL-1.1-ALv2

package grants

import (
	"errors"
	"fmt"
	"testing"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

var seq int

func newID() string { seq++; return fmt.Sprintf("g%d", seq) }

func req(auth_ string, p string, op string) Request {
	return Request{AuthorizationID: auth_, OrgID: "o", SandboxID: "s", DeviceID: "d", ChannelGen: 1,
		Principal: auth.Principal{Kind: ownsandv1.PrincipalKind_PRINCIPAL_KIND_USER, ID: p},
		Kind:      ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC, OperationID: op, Digest: []byte("digest")}
}

const accepted = ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_ACCEPTED

func requireGrantDone(t *testing.T, g *Grant, transition string) {
	t.Helper()
	select {
	case <-g.Done():
	default:
		t.Fatalf("%s did not close the grant notification channel", transition)
	}
}

func TestIdempotencyAndAdmission(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	r := New(c.now)
	g, existing, err := r.Reserve(req("a1", "u", "op"), newID)
	if err != nil || existing {
		t.Fatal(err)
	}
	if r.Admittable(g.ID) != nil {
		t.Fatal("a pending grant is admittable")
	}
	again, existing, _ := r.Reserve(req("a1", "u", "op"), newID)
	if !existing || again.ID != g.ID {
		t.Fatal("retry did not return the same grant")
	}
	if _, _, err := r.Reserve(req("a1", "u", "other"), newID); !errors.Is(err, ErrReuse) {
		t.Fatalf("reuse: %v", err)
	}
	// A changed or added predecessor under a known authorization ID is a
	// reuse of the ID for another request.
	withPred := req("a1", "u", "op")
	withPred.ReplacesID = "gone"
	if _, _, err := r.Reserve(withPred, newID); !errors.Is(err, ErrReuse) {
		t.Fatalf("added predecessor: %v", err)
	}
	r.Ack(g.ID, accepted, "")
	requireGrantDone(t, g, "accepted acknowledgement")
	if r.Admittable(g.ID) == nil {
		t.Fatal("installed grant not admittable")
	}
	// Within and past the admission tolerance.
	c.t = c.t.Add(Lifetime + 29*time.Second)
	if r.Admittable(g.ID) == nil {
		t.Fatal("not admittable within the tolerance")
	}
	c.t = c.t.Add(2 * time.Second)
	if r.Admittable(g.ID) != nil {
		t.Fatal("admittable past the tolerance")
	}
	// Still revocable: it counts and a retry gets a new grant.
	if r.Get(g.ID) == nil {
		t.Fatal("revocable grant forgotten")
	}
	fresh, existing, _ := r.Reserve(req("a1", "u", "op"), newID)
	if existing || fresh.ID == g.ID {
		t.Fatal("expired grant returned for a retry")
	}
	c.t = c.t.Add(StreamDeadline)
	r.Reserve(req("a9", "u", "x"), newID)
	if r.Get(g.ID) != nil {
		t.Fatal("grant kept past the revocation window")
	}
}

func TestReplacementRetryIdempotency(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	r := New(c.now)
	first, _, _ := r.Reserve(req("a0", "u", "op"), newID)
	r.Ack(first.ID, accepted, "")
	other, _, _ := r.Reserve(req("a1", "u", "op"), newID)
	r.Ack(other.ID, accepted, "")

	rep := req("rep", "u", "op")
	rep.ReplacesID = first.ID
	succ, existing, err := r.Reserve(rep, newID)
	if err != nil || existing || succ.Replaces != first.ID {
		t.Fatalf("replacement: %v %v", err, existing)
	}
	// A different predecessor or none under the same authorization ID is a
	// reuse, also after a retry that names the original predecessor.
	changed := rep
	changed.ReplacesID = other.ID
	if _, _, err := r.Reserve(changed, newID); !errors.Is(err, ErrReuse) {
		t.Fatalf("changed predecessor: %v", err)
	}
	dropped := rep
	dropped.ReplacesID = ""
	if _, _, err := r.Reserve(dropped, newID); !errors.Is(err, ErrReuse) {
		t.Fatalf("dropped predecessor: %v", err)
	}
	// An exact retry returns the existing grant, also after the predecessor
	// went away and the grant's resolved link was cleared.
	r.Ack(succ.ID, accepted, first.ID)
	if r.Get(first.ID) != nil {
		t.Fatal("predecessor kept")
	}
	again, existing, err := r.Reserve(rep, newID)
	if err != nil || !existing || again.ID != succ.ID {
		t.Fatalf("exact retry: %v %v", err, existing)
	}
	// A grant that never resolved a predecessor keeps the requested one.
	stale := req("stale", "u", "op")
	stale.ReplacesID = "gone"
	sg, _, err := r.Reserve(stale, newID)
	if err != nil || sg.Replaces != "" {
		t.Fatalf("unresolved predecessor: %v", err)
	}
	if again, existing, err := r.Reserve(stale, newID); err != nil || !existing || again.ID != sg.ID {
		t.Fatalf("retry of an unresolved predecessor: %v", err)
	}
	stale.ReplacesID = ""
	if _, _, err := r.Reserve(stale, newID); !errors.Is(err, ErrReuse) {
		t.Fatalf("dropped unresolved predecessor: %v", err)
	}
}

func TestLimitsReleaseAndRejection(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	r := New(c.now)
	// More than 256 sequential operations on one device succeed when each
	// grant is released.
	for i := 0; i < 300; i++ {
		g, _, err := r.Reserve(req(fmt.Sprintf("seq%d", i), fmt.Sprintf("u%d", i%20), fmt.Sprint(i)), newID)
		if err != nil {
			t.Fatalf("operation %d: %v", i, err)
		}
		r.Ack(g.ID, accepted, "")
		r.Release(g.ID, "d")
	}
	// Per principal: 32.
	for i := 0; i < MaxPerPrincipal; i++ {
		if _, _, err := r.Reserve(req(fmt.Sprintf("p%d", i), "busy", fmt.Sprint(i)), newID); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := r.Reserve(req("p-over", "busy", "x"), newID); !errors.Is(err, ErrLimit) {
		t.Fatalf("per-principal limit: %v", err)
	}
	// Per device: 256.
	for i := 0; i < MaxPerDevice-MaxPerPrincipal; i++ {
		if _, _, err := r.Reserve(req(fmt.Sprintf("d%d", i), fmt.Sprintf("x%d", i), "o"), newID); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := r.Reserve(req("d-over", "someone", "x"), newID); !errors.Is(err, ErrLimit) {
		t.Fatalf("per-device limit: %v", err)
	}
	// An agent rejection removes the grant.
	r2 := New(c.now)
	g, _, _ := r2.Reserve(req("r", "u", "op"), newID)
	r2.Ack(g.ID, ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_REJECTED_GRANT_LIMIT, "")
	requireGrantDone(t, g, "grant limit rejection")
	if g.Outcome() != OutcomeRejectedLimit || r2.Len() != 0 {
		t.Fatal("rejected grant kept")
	}
	// Removal of a pending grant without an acknowledgement (channel loss,
	// release, revocation or another non-accepting result) is not a rejection.
	r3 := New(c.now)
	lost, _, _ := r3.Reserve(req("lost", "u", "op"), newID)
	r3.RemoveChannel(lost.DeviceID, lost.ChannelGen)
	requireGrantDone(t, lost, "channel loss")
	if lost.Outcome() != OutcomeRemoved || r3.Len() != 0 {
		t.Fatalf("channel loss outcome = %v", lost.Outcome())
	}
	released, _, _ := r3.Reserve(req("released", "u", "op2"), newID)
	r3.Release(released.ID, released.DeviceID)
	requireGrantDone(t, released, "release")
	if released.Outcome() != OutcomeRemoved {
		t.Fatalf("release outcome = %v", released.Outcome())
	}
	revoked, _, _ := r3.Reserve(req("revoked", "u", "op3"), newID)
	r3.RemoveMatching(func(x *Grant) bool { return x.ID == revoked.ID })
	requireGrantDone(t, revoked, "matching revocation")
	if revoked.Outcome() != OutcomeRemoved {
		t.Fatalf("revocation outcome = %v", revoked.Outcome())
	}
	odd, _, _ := r3.Reserve(req("odd", "u", "op4"), newID)
	r3.Ack(odd.ID, ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_UNSPECIFIED, "")
	requireGrantDone(t, odd, "unspecified acknowledgement")
	if odd.Outcome() != OutcomeRemoved {
		t.Fatalf("unspecified result outcome = %v", odd.Outcome())
	}
	accepted, _, _ := r3.Reserve(req("accepted", "u", "op5"), newID)
	r3.Ack(accepted.ID, ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_ACCEPTED, "")
	requireGrantDone(t, accepted, "accepted acknowledgement")
	if accepted.Outcome() != OutcomeAccepted {
		t.Fatalf("accepted outcome = %v", accepted.Outcome())
	}
}

func TestReplacementAtTheLimit(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	r := New(c.now)
	var first *Grant
	for i := 0; i < MaxPerPrincipal; i++ {
		g, _, _ := r.Reserve(req(fmt.Sprintf("a%d", i), "u", fmt.Sprint(i)), newID)
		r.Ack(g.ID, accepted, "")
		if i == 0 {
			first = g
		}
	}
	// Two grants for the same exec by the same principal: a separate grant
	// is over the limit, a replacement is not.
	rep := req("rep", "u", "0")
	rep.ReplacesID = first.ID
	succ, _, err := r.Reserve(rep, newID)
	if err != nil || succ.Replaces != first.ID {
		t.Fatalf("replacement at the limit: %v", err)
	}
	// A second replacement of the same predecessor waits or fails.
	rep2 := req("rep2", "u", "0")
	rep2.ReplacesID = first.ID
	if _, _, err := r.Reserve(rep2, newID); !errors.Is(err, ErrReplaceActive) {
		t.Fatalf("second replacement: %v", err)
	}
	// Both stay revocable until the acknowledgement names the swap.
	if r.Get(first.ID) == nil {
		t.Fatal("predecessor dropped before the acknowledgement")
	}
	r.Ack(succ.ID, accepted, first.ID)
	if r.Get(first.ID) != nil || r.Admittable(succ.ID) == nil {
		t.Fatal("swap not applied")
	}
	// A stale replaces_grant_id after channel loss creates a separate grant
	// under the normal limits.
	r.RemoveChannel("d", 1)
	stale := req("stale", "u", "0")
	stale.ReplacesID = succ.ID
	g, _, err := r.Reserve(stale, newID)
	if err != nil || g.Replaces != "" {
		t.Fatalf("stale replacement: %v %+v", err, g)
	}
}

func TestRevocationMatchingAndLateAck(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	r := New(c.now)
	g, _, _ := r.Reserve(req("a", "u", "op"), newID)
	// The acknowledgement arrives late: the grant is still pending and is
	// revoked anyway.
	revoked := r.RemoveMatching(func(g *Grant) bool { return g.Principal.ID == "u" })
	if len(revoked) != 1 || revoked[0].ID != g.ID {
		t.Fatalf("revoked = %v", revoked)
	}
	r.Ack(g.ID, accepted, "")
	if r.Admittable(g.ID) != nil {
		t.Fatal("a late acknowledgement revived a revoked grant")
	}
}

// TestRemoveUndelivered covers the removal after a failed enqueue: the exact
// reservation is released, the outcome is recorded before the waiters wake and
// a replacement leaves its predecessor installed and free of a successor.
func TestRemoveUndelivered(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	r := New(c.now)
	g, _, _ := r.Reserve(req("a", "u", "op"), newID)
	woken := make(chan Outcome, 1)
	go func() {
		select {
		case <-g.Done():
			woken <- g.Outcome()
		case <-t.Context().Done():
		}
	}()
	if !r.RemoveUndelivered(g.ID) {
		t.Fatal("pending grant not removed")
	}
	select {
	case got := <-woken:
		if got != OutcomeDeliveryFailed {
			t.Fatalf("waiter outcome = %v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("RemoveUndelivered did not wake the waiter within one second")
	}
	if r.Len() != 0 || r.Get(g.ID) != nil {
		t.Fatal("removed grant is still registered")
	}
	// The authorization_id slot is free: the same ID reserves a new grant.
	again, existing, err := r.Reserve(req("a", "u", "op"), newID)
	if err != nil || existing || again.ID == g.ID {
		t.Fatalf("reserve after removal: %v existing=%v same=%v", err, existing, again.ID == g.ID)
	}
	// A second removal of the removed grant changes nothing.
	if r.RemoveUndelivered(g.ID) || r.Len() != 1 {
		t.Fatal("removal of a removed grant touched the registry")
	}
	// An installed grant is never removed.
	r.Ack(again.ID, accepted, "")
	if r.RemoveUndelivered(again.ID) || r.Admittable(again.ID) == nil {
		t.Fatal("installed grant removed")
	}
	// A failed replacement keeps the predecessor and frees its successor link.
	repl := req("b", "u", "op")
	repl.ReplacesID = again.ID
	succ, _, err := r.Reserve(repl, newID)
	if err != nil || succ.Replaces != again.ID {
		t.Fatalf("replacement: %v", err)
	}
	second := req("c", "u", "op")
	second.ReplacesID = again.ID
	if _, _, err := r.Reserve(second, newID); !errors.Is(err, ErrReplaceActive) {
		t.Fatalf("second replacement: %v", err)
	}
	if !r.RemoveUndelivered(succ.ID) {
		t.Fatal("pending replacement not removed")
	}
	if succ.Outcome() != OutcomeDeliveryFailed {
		t.Fatalf("replacement outcome = %v", succ.Outcome())
	}
	if r.Admittable(again.ID) == nil || again.Successor != "" || r.Len() != 1 {
		t.Fatalf("predecessor not kept clean: successor=%q len=%d", again.Successor, r.Len())
	}
	if _, _, err := r.Reserve(repl, newID); err != nil {
		t.Fatalf("replacement retry after the failed one: %v", err)
	}
}

// installedWithReplacement installs a grant and reserves a pending
// replacement of it.
func installedWithReplacement(t *testing.T, r *Registry) (pred, succ *Grant) {
	t.Helper()
	pred, _, err := r.Reserve(req("pred", "u", "op"), newID)
	if err != nil {
		t.Fatal(err)
	}
	r.Ack(pred.ID, accepted, "")
	repl := req("succ", "u", "op")
	repl.ReplacesID = pred.ID
	succ, _, err = r.Reserve(repl, newID)
	if err != nil || succ.Replaces != pred.ID {
		t.Fatalf("replacement: %v", err)
	}
	return pred, succ
}

// TestAckWithoutReplacedGrantFreesPredecessor covers an accepting
// acknowledgement that does not name the predecessor: the predecessor stays
// installed and can be replaced again.
func TestAckWithoutReplacedGrantFreesPredecessor(t *testing.T) {
	for _, replacedID := range []string{"", "another-grant"} {
		t.Run("replaced="+replacedID, func(t *testing.T) {
			c := &clock{t: time.Unix(1000, 0)}
			r := New(c.now)
			pred, succ := installedWithReplacement(t, r)
			// While the successor is pending, a second replacement is refused.
			again := req("again", "u", "op")
			again.ReplacesID = pred.ID
			if _, _, err := r.Reserve(again, newID); !errors.Is(err, ErrReplaceActive) {
				t.Fatalf("replacement while pending: %v", err)
			}
			r.Ack(succ.ID, accepted, replacedID)
			if r.Get(pred.ID) == nil {
				t.Fatal("predecessor removed by an acknowledgement that did not name it")
			}
			if pred.Successor != "" || succ.Replaces != "" {
				t.Fatalf("links not cleared: successor=%q replaces=%q", pred.Successor, succ.Replaces)
			}
			// The predecessor can be replaced again.
			next, _, err := r.Reserve(again, newID)
			if err != nil || next.Replaces != pred.ID {
				t.Fatalf("replacement after the ack: %v", err)
			}
		})
	}
}

// TestAckNamingPredecessorRemovesIt keeps the contract path: the named
// predecessor is removed and the successor stays installed.
func TestAckNamingPredecessorRemovesIt(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	r := New(c.now)
	pred, succ := installedWithReplacement(t, r)
	r.Ack(succ.ID, accepted, pred.ID)
	requireGrantDone(t, succ, "accepted acknowledgement naming the predecessor")
	if r.Get(pred.ID) != nil || r.Admittable(succ.ID) == nil || succ.Replaces != "" {
		t.Fatal("swap not applied")
	}
}

// TestReplacesOf covers the locked snapshot of the predecessor link.
func TestReplacesOf(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	r := New(c.now)
	pred, succ := installedWithReplacement(t, r)
	if got := r.ReplacesOf(succ); got != pred.ID {
		t.Fatalf("ReplacesOf = %q", got)
	}
	r.Release(pred.ID, pred.DeviceID)
	if got := r.ReplacesOf(succ); got != "" {
		t.Fatalf("ReplacesOf after the release = %q", got)
	}
	if r.ReplacesOf(pred) != "" {
		t.Fatal("grant without predecessor reports one")
	}
}

// TestReplacesOfConcurrentRelease runs the snapshot against releases and acks
// of the link; the race detector reports an unlocked read.
func TestReplacesOfConcurrentRelease(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	r := New(c.now)
	pred, succ := installedWithReplacement(t, r)
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Release(pred.ID, pred.DeviceID)
		r.Ack(succ.ID, accepted, pred.ID)
	}()
	for i := 0; i < 1000; i++ {
		if got := r.ReplacesOf(succ); got != "" && got != pred.ID {
			t.Fatalf("ReplacesOf = %q", got)
		}
	}
	<-done
}

// TestRevocableUntil pins the end of the revocable time of a grant: the stream
// deadline plus the delivery margin, or the acknowledgement plus the sent
// stream deadline duration when that is later.
func TestRevocableUntil(t *testing.T) {
	start := time.Unix(1000, 0)
	ackTime := start.Add(2 * time.Minute)
	cutoff := ackTime.Add(StreamDeadline)
	for _, tc := range []struct {
		name        string
		removeAt    time.Time
		wantMatches int
	}{
		{name: "before cutoff", removeAt: cutoff.Add(-time.Second), wantMatches: 1},
		{name: "at cutoff", removeAt: cutoff, wantMatches: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &clock{t: start}
			r := New(c.now)
			g, _, err := r.Reserve(req("a1", "u", "op"), newID)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := g.RevocableUntil(), start.Add(StreamDeadline+DeliveryMargin); !got.Equal(want) {
				t.Fatalf("pending grant revocable until %v, want %v", got, want)
			}

			c.t = ackTime
			r.Ack(g.ID, accepted, "")
			c.t = tc.removeAt
			registered := r.Get(g.ID)
			if registered == nil || registered.ID != g.ID || registered.State != Installed {
				t.Fatalf("expected installed grant %q in the registry before RemoveMatching, got %v", g.ID, registered)
			}
			if got := registered.RevocableUntil(); !got.Equal(cutoff) {
				t.Fatalf("acknowledged grant revocable until %v, want %v", got, cutoff)
			}

			matches := r.RemoveMatching(func(*Grant) bool { return true })
			if len(matches) != tc.wantMatches {
				t.Fatalf("RemoveMatching returned %d matches, want %d", len(matches), tc.wantMatches)
			}
			for _, match := range matches {
				if match.ID != g.ID || !match.RevocableUntil().Equal(cutoff) {
					t.Fatalf("RemoveMatching returned grant %q revocable until %v, want grant %q revocable until %v", match.ID, match.RevocableUntil(), g.ID, cutoff)
				}
			}
		})
	}
}

func TestGrantRetryKeepsMembershipSnapshot(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	r := New(c.now)
	request := req("authorization", "user", "operation")
	request.Membership = auth.MembershipSnapshot{ID: "membership", DeliveryID: 10}
	original, _, err := r.Reserve(request, newID)
	if err != nil {
		t.Fatal(err)
	}
	r.Ack(original.ID, accepted, "")

	retry := request
	retry.Membership.DeliveryID++
	g, existing, err := r.Reserve(retry, newID)
	if err != nil || !existing || g != original {
		t.Fatalf("retry = %v, existing = %t, error = %v", g, existing, err)
	}
	if g.Membership != request.Membership {
		t.Fatalf("stored membership = %+v, want %+v", g.Membership, request.Membership)
	}

	rejoined := retry
	rejoined.Membership.ID = "replacement"
	if _, _, err := r.Reserve(rejoined, newID); !errors.Is(err, ErrReuse) {
		t.Fatalf("retry from replacement membership: %v", err)
	}
	// Streams can still be live after admission expires. Keep the conflict
	// while the original grant is tracked, even when it cannot admit streams.
	c.t = c.t.Add(Lifetime + AdmissionTolerance)
	if _, _, err := r.Reserve(rejoined, newID); !errors.Is(err, ErrReuse) {
		t.Fatalf("retry from replacement membership after admission expiry: %v", err)
	}
	g, existing, err = r.Reserve(retry, newID)
	if err != nil || existing || g.ID == original.ID {
		t.Fatalf("same-membership retry after admission expiry = %v, existing = %t, error = %v", g, existing, err)
	}
}

func TestGrantRequestBinding(t *testing.T) {
	for _, state := range []struct {
		name      string
		installed bool
		advance   time.Duration
	}{
		{name: "pending"},
		{name: "admittable", installed: true},
		{name: "admission expired", installed: true, advance: Lifetime + AdmissionTolerance},
	} {
		t.Run(state.name, func(t *testing.T) {
			c := &clock{t: time.Unix(1000, 0)}
			r := New(c.now)
			request := req("authorization", "user", "operation")
			request.Membership.ID = "membership"
			original, _, err := r.Reserve(request, newID)
			if err != nil {
				t.Fatal(err)
			}
			if state.installed {
				r.Ack(original.ID, accepted, "")
			}
			c.t = c.t.Add(state.advance)

			for _, change := range []struct {
				name string
				edit func(*Request)
			}{
				{"principal", func(r *Request) { r.Principal.ID = "other" }},
				{"membership", func(r *Request) { r.Membership.ID = "other" }},
				{"organization", func(r *Request) { r.OrgID = "other" }},
				{"sandbox", func(r *Request) { r.SandboxID = "other" }},
				{"device", func(r *Request) { r.DeviceID = "other" }},
				{"operation kind", func(r *Request) { r.Kind = ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_WRITE_FILE }},
				{"operation ID", func(r *Request) { r.OperationID = "other" }},
				{"digest", func(r *Request) { r.Digest = []byte("other") }},
				{"predecessor", func(r *Request) { r.ReplacesID = "other" }},
			} {
				t.Run(change.name, func(t *testing.T) {
					conflict := request
					change.edit(&conflict)
					if g, existing, err := r.Reserve(conflict, newID); g != nil || existing || !errors.Is(err, ErrReuse) {
						t.Fatalf("conflicting reservation: grant = %v, existing = %t, error = %v", g, existing, err)
					}
					if r.byAuth[request.AuthorizationID] != original || r.Get(original.ID) != original || r.Len() != 1 {
						t.Fatal("conflict changed the original registration")
					}
				})
			}

			if state.advance == 0 {
				if g, existing, err := r.Reserve(request, newID); err != nil || !existing || g != original {
					t.Fatalf("identical retry: grant = %v, existing = %t, error = %v", g, existing, err)
				}
			}
			c.t = original.RevocableUntil()
			changed := request
			changed.OperationID = "operation after removal"
			if g, existing, err := r.Reserve(changed, newID); err != nil || existing || g == original || r.Get(original.ID) != nil {
				t.Fatalf("reservation after sweep: grant = %v, existing = %t, error = %v", g, existing, err)
			}
		})
	}
}

func TestGrantFailedRetryKeepsRequestBinding(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	r := New(c.now)
	request := req("authorization", "user", "operation")
	original, _, err := r.Reserve(request, newID)
	if err != nil {
		t.Fatal(err)
	}
	r.Ack(original.ID, accepted, "")
	c.t = c.t.Add(Lifetime + AdmissionTolerance)

	var last *Grant
	for i := 1; i < MaxPerPrincipal; i++ {
		last, _, err = r.Reserve(req(fmt.Sprintf("filler-%d", i), "user", "filler"), newID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := r.Reserve(request, newID); !errors.Is(err, ErrLimit) {
		t.Fatalf("identical retry at the limit: %v", err)
	}
	r.Release(last.ID, request.DeviceID)
	conflict := request
	conflict.OperationID = "other"
	if _, _, err := r.Reserve(conflict, newID); !errors.Is(err, ErrReuse) {
		t.Fatalf("conflict after a failed retry: %v", err)
	}
	if r.byAuth[request.AuthorizationID] != original || r.Get(original.ID) != original {
		t.Fatal("failed retries changed the original registration")
	}
	if g, existing, err := r.Reserve(request, newID); err != nil || existing || g == original {
		t.Fatalf("identical retry with room: grant = %v, existing = %t, error = %v", g, existing, err)
	}
}
