// SPDX-License-Identifier: FSL-1.1-ALv2

package store

import (
	"testing"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestCompareRevision(t *testing.T) {
	cases := []struct {
		dev, rep string
		rc       uint64
		stored   string
		sc       uint64
		want     RevisionOrder
	}{
		{"e2", "e2", 1, "e1", 99, RevisionNewer}, // a newer epoch always wins
		{"e2", "e1", 100, "e1", 99, RevisionOlder},
		{"e1", "e1", 5, "e1", 4, RevisionNewer},
		{"e1", "e1", 4, "e1", 4, RevisionEqual},
		{"e1", "e1", 3, "e1", 4, RevisionOlder},
		{"e1", "e1", 1, "", 0, RevisionNewer}, // never reported
	}
	for i, c := range cases {
		if got := CompareRevision(c.dev, c.rep, c.rc, c.stored, c.sc); got != c.want {
			t.Errorf("case %d: got %d want %d", i, got, c.want)
		}
	}
}

func TestDisplayState(t *testing.T) {
	R, D, U := ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, ownsandv1.SandboxState_SANDBOX_STATE_DELETING, ownsandv1.SandboxState_SANDBOX_STATE_UNREACHABLE
	F := ownsandv1.SandboxState_SANDBOX_STATE_FAILED
	check := func(agent ownsandv1.SandboxState, pending, online bool, wantState, wantLast ownsandv1.SandboxState) {
		t.Helper()
		s, l := DisplayState(agent, pending, online)
		if s != wantState || l != wantLast {
			t.Errorf("%v pending=%v online=%v: got %v/%v", agent, pending, online, s, l)
		}
	}
	check(R, false, true, R, 0)
	check(R, false, false, U, R)
	check(R, true, true, D, 0)
	check(R, true, false, U, D)
	check(F, true, false, F, 0)
}
