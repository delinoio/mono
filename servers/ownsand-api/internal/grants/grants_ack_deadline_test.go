// SPDX-License-Identifier: FSL-1.1-ALv2

package grants

import (
	"testing"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

const rejectedLimit = ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_REJECTED_GRANT_LIMIT

// TestAckAdmissionDeadline covers an acknowledgement around the admission
// deadline of a pending grant (ExpireAt + AdmissionTolerance): inside the
// window it installs the grant, at or after it removes the grant, because
// Admittable would reject the installed grant at once.
func TestAckAdmissionDeadline(t *testing.T) {
	const window = Lifetime + AdmissionTolerance
	cases := []struct {
		name      string
		elapsed   time.Duration
		installed bool
	}{
		{name: "just before the deadline", elapsed: window - time.Nanosecond, installed: true},
		{name: "exactly at the deadline", elapsed: window, installed: false},
		{name: "just after the deadline", elapsed: window + time.Nanosecond, installed: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &clock{t: time.Unix(1000, 0)}
			r := New(c.now)
			g, _, err := r.Reserve(req("a", "u", "op"), newID)
			if err != nil {
				t.Fatal(err)
			}
			c.t = c.t.Add(tc.elapsed)
			r.Ack(g.ID, accepted, "")
			select {
			case <-g.Done():
			default:
				t.Fatal("the acknowledgement did not wake the waiters")
			}
			if tc.installed {
				if g.Outcome() != OutcomeAccepted || r.Get(g.ID) == nil {
					t.Fatalf("grant not installed: outcome=%v", g.Outcome())
				}
				if r.Admittable(g.ID) == nil {
					t.Fatal("the installed grant is not admittable inside the window")
				}
				return
			}
			if g.Outcome() != OutcomeRemoved {
				t.Fatalf("outcome = %v, want OutcomeRemoved", g.Outcome())
			}
			if r.Get(g.ID) != nil || r.Admittable(g.ID) != nil || r.Len() != 0 {
				t.Fatal("the expired grant is still registered")
			}
			// The authorization ID is released: the same request reserves a
			// fresh grant instead of returning the expired one.
			fresh, existing, err := r.Reserve(req("a", "u", "op"), newID)
			if err != nil || existing || fresh.ID == g.ID {
				t.Fatalf("reserve after the expiry: err=%v existing=%v same=%v", err, existing, fresh.ID == g.ID)
			}
		})
	}
}

// TestAckAfterDeadlineIgnoresResult covers the precedence of the deadline over
// the acknowledged result: a limit rejection after the deadline is still an
// ordinary removal, not a capacity rejection.
func TestAckAfterDeadlineIgnoresResult(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	r := New(c.now)
	g, _, _ := r.Reserve(req("a", "u", "op"), newID)
	c.t = c.t.Add(Lifetime + AdmissionTolerance)
	r.Ack(g.ID, rejectedLimit, "")
	if g.Outcome() != OutcomeRemoved {
		t.Fatalf("outcome = %v, want OutcomeRemoved", g.Outcome())
	}
}

// TestAckAfterDeadlineReleasesReplacementLink covers a pending replacement
// that is acknowledged after its deadline: the predecessor stays registered
// and loses its pending-successor link, so it can be replaced again.
func TestAckAfterDeadlineReleasesReplacementLink(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	r := New(c.now)
	pred, succ := installedWithReplacement(t, r)
	c.t = c.t.Add(Lifetime + AdmissionTolerance)
	r.Ack(succ.ID, accepted, pred.ID)
	if succ.Outcome() != OutcomeRemoved || r.Get(succ.ID) != nil {
		t.Fatalf("expired replacement kept: outcome=%v", succ.Outcome())
	}
	if r.Get(pred.ID) == nil {
		t.Fatal("the predecessor was removed by an expired replacement")
	}
	if pred.Successor != "" {
		t.Fatalf("predecessor still links the removed replacement: %q", pred.Successor)
	}
}
