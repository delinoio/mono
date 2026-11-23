// SPDX-License-Identifier: FSL-1.1-ALv2

package billing_test

import (
	"context"
	"testing"
	"time"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/billing"
)

// blocked, suspended and allowed shorten the account statuses of the tests.
const (
	remoteBlocked   = accountsv1.BillingStatus_BILLING_STATUS_BLOCKED
	remoteSuspended = accountsv1.BillingStatus_BILLING_STATUS_SUSPENDED
	remoteAllowed   = accountsv1.BillingStatus_BILLING_STATUS_ALLOWED
)

// equalTime reports whether got is a non-nil time equal to want.
func equalTime(got *time.Time, want time.Time) bool { return got != nil && got.Equal(want) }

// poll runs one poll of the fixture.
func (f *statusFixture) poll(t *testing.T) {
	t.Helper()
	if err := f.statuses.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestRecoveryAndNewBlockBetweenPollsRestartsTheClock checks that a link in
// non-payment whose account recovers and is blocked again before the next
// poll starts a new local block interval: OwnSand only sees the newest
// status (BLOCKED with the new start), and the 3-day suspension clock of the
// new interval runs from that start instead of from the earlier one.
func TestRecoveryAndNewBlockBetweenPollsRestartsTheClock(t *testing.T) {
	t0 := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	f := newStatusFixture(t, t0)
	f.addOrg(t, org1)
	f.link(t, org1, accountA)

	first := t0.Add(time.Hour)
	f.clock.Set(first)
	f.fake.SetAccountStatus(accountA, remoteBlocked, first)
	f.poll(t)
	st := f.load(t, org1)
	if st.Phase != billing.PhaseBlocked || !equalTime(st.BlockedSince, first) || !equalTime(st.RemoteBlockedSince, first) {
		t.Fatalf("first interval = %+v", st)
	}

	// The account recovers and is blocked again; no poll runs in between,
	// so the control plane never sees the ALLOWED status.
	f.clock.Set(first.Add(24 * time.Hour))
	f.fake.SetAccountStatus(accountA, remoteAllowed, time.Time{})
	second := first.Add(36 * time.Hour)
	f.clock.Set(second)
	f.fake.SetAccountStatus(accountA, remoteBlocked, second)
	f.poll(t)
	st = f.load(t, org1)
	if st.Phase != billing.PhaseBlocked || !equalTime(st.BlockedSince, second) || !equalTime(st.RemoteBlockedSince, second) {
		t.Fatalf("second interval = %+v, want the block start %v", st, second)
	}
	if got, want := st.SuspendTime(), second.Add(billing.SuspendAfter); !got.Equal(want) {
		t.Fatalf("suspend time = %v, want %v", got, want)
	}
	// The deadline of the first interval passes without a suspension.
	if got := st.Confirmed(first.Add(billing.SuspendAfter)); got != billing.PhaseBlocked {
		t.Fatalf("at the first deadline = %v, want BLOCKED", got)
	}
	if got := st.Confirmed(second.Add(billing.SuspendAfter)); got != billing.PhaseSuspended {
		t.Fatalf("at the second deadline = %v, want SUSPENDED", got)
	}
}

// TestContinuingBlockKeepsItsStart checks that the same non-payment keeps its
// start when the account status changes from BLOCKED to SUSPENDED, when the
// same status is reported again and when a status without a block start
// arrives.
func TestContinuingBlockKeepsItsStart(t *testing.T) {
	t0 := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	f := newStatusFixture(t, t0)
	ctx := context.Background()
	f.addOrg(t, org1)
	f.link(t, org1, accountA)
	start := t0.Add(time.Hour)
	f.clock.Set(start)
	f.fake.SetAccountStatus(accountA, remoteBlocked, start)
	f.poll(t)
	seq := f.load(t, org1).Sequence

	// The same interval changes to SUSPENDED with a new sequence.
	f.clock.Set(start.Add(billing.SuspendAfter))
	f.fake.SetAccountStatus(accountA, remoteSuspended, start)
	f.poll(t)
	st := f.load(t, org1)
	if st.Sequence <= seq || st.Phase != billing.PhaseSuspended || !equalTime(st.BlockedSince, start) || !equalTime(st.RemoteBlockedSince, start) {
		t.Fatalf("SUSPENDED of the same interval = %+v", st)
	}

	// A repeated poll and the same status with the applied sequence change
	// nothing.
	f.clock.Advance(time.Minute)
	f.poll(t)
	f.statuses.ApplyReportStatuses(ctx, []*accountsv1.OrganizationBillingStatus{f.fake.Status(org1)})
	if got := f.load(t, org1); !equalTime(got.BlockedSince, start) || got.Sequence != st.Sequence || got.Phase != billing.PhaseSuspended {
		t.Fatalf("repeated status = %+v", got)
	}

	// A later status without a block start is no new interval and keeps the
	// stored reported start.
	f.statuses.ApplyReportStatuses(ctx, []*accountsv1.OrganizationBillingStatus{{Service: billing.ServiceID, OrganizationId: org1, BillingAccountId: accountA,
		Status: remoteBlocked, Sequence: st.Sequence + 1}})
	if got := f.load(t, org1); !equalTime(got.BlockedSince, start) || !equalTime(got.RemoteBlockedSince, start) {
		t.Fatalf("status without a block start = %+v", got)
	}
}

// TestLinkAfterUnlinkKeepsEarlierStartAndLaterPollsDoNotReset checks the
// documented rule that an unlink followed by a link to a billing account in
// non-payment keeps the earlier local block start, and that neither a later
// status of the same remote interval nor a repeated poll moves it. Only a
// new remote interval after the link restarts the clock.
func TestLinkAfterUnlinkKeepsEarlierStartAndLaterPollsDoNotReset(t *testing.T) {
	t0 := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	f := newStatusFixture(t, t0)
	f.addOrg(t, org1)
	// The organization has been blocked locally since t0 (creation). The
	// account is in non-payment since the link time, which is later.
	f.clock.Advance(24 * time.Hour)
	f.fake.SetAccountStatus(accountB, remoteBlocked, t0.Add(-time.Hour))
	st := f.link(t, org1, accountB)
	reported := f.fake.Status(org1).GetBlockedSince().AsTime()
	if !reported.After(t0) || !equalTime(st.BlockedSince, t0) || !equalTime(st.RemoteBlockedSince, reported) {
		t.Fatalf("link in non-payment = %+v (reported start %v)", st, reported)
	}

	// Unlink and link again: the reported start is not an earlier interval
	// of the new link, so the earlier local start stays.
	f.removeLink(t, org1, billing.SourceUnlink)
	if got := f.load(t, org1); got.RemoteBlockedSince != nil || !equalTime(got.BlockedSince, t0) {
		t.Fatalf("after unlink = %+v", got)
	}
	f.clock.Advance(time.Hour)
	st = f.link(t, org1, accountB)
	if !equalTime(st.BlockedSince, t0) || !equalTime(st.RemoteBlockedSince, reported) {
		t.Fatalf("after relink = %+v", st)
	}

	// The same remote interval later becomes SUSPENDED, and a poll repeats:
	// the local start stays.
	f.fake.SetAccountStatus(accountB, remoteSuspended, t0.Add(-time.Hour))
	f.poll(t)
	f.poll(t)
	st = f.load(t, org1)
	if st.Phase != billing.PhaseSuspended || !equalTime(st.BlockedSince, t0) || !equalTime(st.RemoteBlockedSince, reported) {
		t.Fatalf("same interval after the relink = %+v", st)
	}

	// A recovery and a new block after the link is a new interval.
	f.fake.SetAccountStatus(accountB, remoteAllowed, time.Time{})
	f.clock.Advance(48 * time.Hour)
	next := f.clock.Now()
	f.fake.SetAccountStatus(accountB, remoteBlocked, next)
	f.poll(t)
	st = f.load(t, org1)
	if st.Phase != billing.PhaseBlocked || !equalTime(st.BlockedSince, next) || !equalTime(st.RemoteBlockedSince, next) {
		t.Fatalf("new interval after the relink = %+v, want the block start %v", st, next)
	}
}

// TestRemoteBlockStartIsForgottenWithTheLinkState checks that an ALLOWED
// status, an unlink and a link to another billing account clear the stored
// reported start, so it never classifies a status of a later link.
func TestRemoteBlockStartIsForgottenWithTheLinkState(t *testing.T) {
	t0 := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	f := newStatusFixture(t, t0)
	f.addOrg(t, org1)
	f.link(t, org1, accountA)
	f.fake.SetAccountStatus(accountA, remoteBlocked, t0)
	f.poll(t)
	if got := f.load(t, org1); !equalTime(got.RemoteBlockedSince, t0) {
		t.Fatalf("blocked = %+v", got)
	}

	f.fake.SetAccountStatus(accountA, remoteAllowed, time.Time{})
	f.poll(t)
	if got := f.load(t, org1); got.Phase != billing.PhaseAllowed || got.RemoteBlockedSince != nil || got.BlockedSince != nil {
		t.Fatalf("allowed = %+v", got)
	}

	f.fake.SetAccountStatus(accountA, remoteBlocked, t0.Add(time.Hour))
	f.poll(t)
	if got := f.load(t, org1); !equalTime(got.RemoteBlockedSince, t0.Add(time.Hour)) {
		t.Fatalf("blocked again = %+v", got)
	}
	f.removeLink(t, org1, billing.SourceUnlink)
	if got := f.load(t, org1); got.Linked() || got.RemoteBlockedSince != nil {
		t.Fatalf("unlinked = %+v", got)
	}
}
