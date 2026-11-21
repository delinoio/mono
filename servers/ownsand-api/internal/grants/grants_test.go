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
	r.Ack(g.ID, accepted, "")
	<-g.Done()
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
	<-g.Done()
	if g.Outcome() != OutcomeRejectedLimit || r2.Len() != 0 {
		t.Fatal("rejected grant kept")
	}
	// Removal of a pending grant without an acknowledgement (channel loss,
	// release, revocation or another non-accepting result) is not a rejection.
	r3 := New(c.now)
	lost, _, _ := r3.Reserve(req("lost", "u", "op"), newID)
	r3.RemoveChannel(lost.DeviceID, lost.ChannelGen)
	<-lost.Done()
	if lost.Outcome() != OutcomeRemoved || r3.Len() != 0 {
		t.Fatalf("channel loss outcome = %v", lost.Outcome())
	}
	released, _, _ := r3.Reserve(req("released", "u", "op2"), newID)
	r3.Release(released.ID, released.DeviceID)
	<-released.Done()
	if released.Outcome() != OutcomeRemoved {
		t.Fatalf("release outcome = %v", released.Outcome())
	}
	revoked, _, _ := r3.Reserve(req("revoked", "u", "op3"), newID)
	r3.RemoveMatching(func(x *Grant) bool { return x.ID == revoked.ID })
	<-revoked.Done()
	if revoked.Outcome() != OutcomeRemoved {
		t.Fatalf("revocation outcome = %v", revoked.Outcome())
	}
	odd, _, _ := r3.Reserve(req("odd", "u", "op4"), newID)
	r3.Ack(odd.ID, ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_UNSPECIFIED, "")
	<-odd.Done()
	if odd.Outcome() != OutcomeRemoved {
		t.Fatalf("unspecified result outcome = %v", odd.Outcome())
	}
	accepted, _, _ := r3.Reserve(req("accepted", "u", "op5"), newID)
	r3.Ack(accepted.ID, ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_ACCEPTED, "")
	<-accepted.Done()
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
	go func() { <-g.Done(); woken <- g.Outcome() }()
	if !r.RemoveUndelivered(g.ID) {
		t.Fatal("pending grant not removed")
	}
	if got := <-woken; got != OutcomeDeliveryFailed {
		t.Fatalf("waiter outcome = %v", got)
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
