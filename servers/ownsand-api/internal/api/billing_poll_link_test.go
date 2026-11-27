// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/accounts/accountstest"
	"github.com/delinoio/mono/servers/ownsand-api/internal/billing"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

func (f *linkAttemptFixture) pollCheckpoint(t *testing.T) (int64, time.Time) {
	t.Helper()
	var sequence int64
	var pollTime time.Time
	if err := f.env.DB.Pool.QueryRow(t.Context(),
		"SELECT after_sequence, poll_time FROM billing_status_cursor WHERE singleton").Scan(&sequence, &pollTime); err != nil {
		t.Fatal(err)
	}
	return sequence, pollTime
}

func (f *linkAttemptFixture) cachedStatus(t *testing.T, orgID string) billing.Status {
	t.Helper()
	st, err := f.env.App.Billing.Load(t.Context(), f.env.DB.Pool, orgID)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestBillingPollDefersFirstLink(t *testing.T) {
	f := newLinkAttemptFixture(t, "first-link-owner")
	before := linkedOrg(t, f.env, f.fake, "before-owner", billingAccountB)
	after := linkedOrg(t, f.env, f.fake, "after-owner", billingAccountC)
	pollBilling(t, f.env)
	_, initialPollTime := f.pollCheckpoint(t)
	initialAfterSyncTime := f.cachedStatus(t, after.ID).SyncTime

	f.fake.SetAccountStatus(billingAccountA, accountsv1.BillingStatus_BILLING_STATUS_BLOCKED, f.env.Clock.Now())
	gate := f.fake.HoldLinkReply()
	t.Cleanup(gate.Release)
	done := make(chan error, 1)
	go func() { done <- f.linkWithContext(t.Context(), f.org.OwnerClient, billingAccountA) }()
	gate.WaitForEntry(t, done)
	captured := f.fake.Status(f.org.ID)

	// Elapsing the call deadline leaves the attempt eligible for completion
	// until another transaction takes it over.
	f.env.Clock.Advance(time.Minute)
	f.fake.SetAccountStatus(billingAccountB, accountsv1.BillingStatus_BILLING_STATUS_BLOCKED, f.env.Clock.Now())
	f.fake.SetAccountStatus(billingAccountA, accountsv1.BillingStatus_BILLING_STATUS_ALLOWED, time.Time{})
	f.fake.SetAccountStatus(billingAccountC, accountsv1.BillingStatus_BILLING_STATUS_BLOCKED, f.env.Clock.Now())

	pollBilling(t, f.env)
	if cursor, pollTime := f.pollCheckpoint(t); cursor != f.fake.Status(before.ID).GetSequence() || !pollTime.Equal(initialPollTime) {
		t.Fatalf("incomplete poll checkpoint = (%d, %v), want (%d, %v)", cursor, pollTime, f.fake.Status(before.ID).GetSequence(), initialPollTime)
	}
	f.wantLocalLink(t, false)
	if st := billingStatus(t, before.OwnerClient, before.ID); st.GetPhase() != billing.PhaseBlocked {
		t.Fatalf("status before deferred result = %v, want BLOCKED", st)
	}
	if st := f.cachedStatus(t, after.ID); st.Phase != billing.PhaseAllowed || st.SyncTime == nil || initialAfterSyncTime == nil || !st.SyncTime.Equal(*initialAfterSyncTime) {
		t.Fatalf("status after deferred result = %+v, want ALLOWED and sync time %v", st, initialAfterSyncTime)
	}

	t.Logf("retained checkpoint %d before deferred sequence %d, with unchanged poll and later row sync times",
		f.fake.Status(before.ID).GetSequence(), f.fake.Status(f.org.ID).GetSequence())
	gate.Release()
	if err := gate.WaitForResult(t, done); err != nil {
		t.Fatal(err)
	}
	if st := f.cachedStatus(t, f.org.ID); st.Phase != billing.PhaseBlocked || st.Sequence != captured.GetSequence() {
		t.Fatalf("after link = %+v, want BLOCKED at %d", st, captured.GetSequence())
	}

	token := &ownsandv1.CreateEnrollmentTokenRequest{OrganizationId: f.org.ID, Ttl: durationpb.New(time.Hour)}
	if _, err := f.org.OwnerClient.Device.CreateEnrollmentToken(t.Context(), connect.NewRequest(token)); testenv.Code(err) != connect.CodeResourceExhausted {
		t.Fatalf("CreateEnrollmentToken before resumed poll = %v, want resource_exhausted", err)
	}

	// A new worker has only the checkpoint in Postgres.
	f.env.Clock.Advance(time.Second)
	if err := f.restart().Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if cursor, pollTime := f.pollCheckpoint(t); cursor != f.fake.Status(after.ID).GetSequence() || !pollTime.Equal(f.env.Clock.Now()) {
		t.Fatalf("resumed poll checkpoint = (%d, %v), want (%d, %v)", cursor, pollTime, f.fake.Status(after.ID).GetSequence(), f.env.Clock.Now())
	}
	if st := f.cachedStatus(t, f.org.ID); st.Sequence != f.fake.Status(f.org.ID).GetSequence() || st.SyncTime == nil || !st.SyncTime.Equal(f.env.Clock.Now()) {
		t.Fatalf("resumed poll status = %+v, want sequence %d and sync time %v", st, f.fake.Status(f.org.ID).GetSequence(), f.env.Clock.Now())
	}
	if st := billingStatus(t, f.org.OwnerClient, f.org.ID); !st.GetLinked() || st.GetPhase() != billing.PhaseAllowed {
		t.Fatalf("GetBillingStatus after resumed poll = %v, want linked and ALLOWED", st)
	}
	if st := billingStatus(t, after.OwnerClient, after.ID); st.GetPhase() != billing.PhaseBlocked {
		t.Fatalf("status after resumed poll = %v, want BLOCKED", st)
	}
	if _, err := f.org.OwnerClient.Device.CreateEnrollmentToken(t.Context(), connect.NewRequest(token)); err != nil {
		t.Fatalf("CreateEnrollmentToken after resumed poll: %v", err)
	}
	t.Logf("resumed poll reached sequence %d, returned ALLOWED and permitted CreateEnrollmentToken",
		f.fake.Status(after.ID).GetSequence())
}

func TestBillingPollSkipsObsoleteFirstLink(t *testing.T) {
	t.Run("different account", func(t *testing.T) {
		f := newLinkAttemptFixture(t, "different-account-owner")
		f.fake.AddAccount(billingAccountB, "Old account", map[string]accountsv1.BillingAccountRole{
			f.org.Owner: accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_OWNER,
		})
		if _, err := f.fake.Client().LinkOrganization(t.Context(), connect.NewRequest(&accountsv1.LinkOrganizationRequest{
			Service: billing.ServiceID, OrganizationId: f.org.ID, BillingAccountId: billingAccountB, UserId: f.org.Owner,
		})); err != nil {
			t.Fatal(err)
		}
		gate := f.fake.Hold(accountstest.MethodLinkOrganization)
		t.Cleanup(gate.Release)
		done := make(chan error, 1)
		go func() { done <- f.linkWithContext(t.Context(), f.org.OwnerClient, billingAccountA) }()
		gate.WaitForEntry(t, done)
		f.fake.SetAccountStatus(billingAccountB, accountsv1.BillingStatus_BILLING_STATUS_BLOCKED, f.env.Clock.Now())

		pollBilling(t, f.env)
		if cursor, _ := f.pollCheckpoint(t); cursor != f.fake.Status(f.org.ID).GetSequence() {
			t.Fatalf("wrong-account status left cursor at %d", cursor)
		}
		f.wantLocalLink(t, false)
		gate.Release()
		if err := gate.WaitForResult(t, done); testenv.Code(err) != connect.CodeAlreadyExists {
			t.Fatalf("attempt to link a different account = %v, want already_exists", err)
		}
		f.wantRows(t, 0, 0)
	})

	t.Run("ownership takeover", func(t *testing.T) {
		f := newLinkAttemptFixture(t, "takeover-owner")
		pollBilling(t, f.env)
		gate := f.fake.HoldLinkReply()
		t.Cleanup(gate.Release)
		done := make(chan error, 1)
		go func() { done <- f.linkWithContext(t.Context(), f.org.OwnerClient, billingAccountA) }()
		gate.WaitForEntry(t, done)
		sequence := f.fake.Status(f.org.ID).GetSequence()
		pollBilling(t, f.env)
		if cursor, _ := f.pollCheckpoint(t); cursor >= sequence {
			t.Fatalf("cursor before takeover = %d, want less than %d", cursor, sequence)
		}

		if _, err := f.org.OwnerClient.Organization.TransferOwnership(t.Context(), connect.NewRequest(&ownsandv1.TransferOwnershipRequest{
			OrganizationId: f.org.ID, UserId: f.org.Admin,
		})); err != nil {
			t.Fatal(err)
		}
		f.wantRows(t, 0, 1)
		f.wantRemoteLink(t, true)
		pollBilling(t, f.env)
		if cursor, _ := f.pollCheckpoint(t); cursor != sequence {
			t.Fatalf("cursor after takeover = %d, want %d", cursor, sequence)
		}
		gate.Release()
		if err := gate.WaitForResult(t, done); testenv.Code(err) != connect.CodeAborted {
			t.Fatalf("superseded completion = %v, want aborted", err)
		}
		f.wantLocalLink(t, false)
	})
}

func TestBillingPollAheadOfRepeatedLink(t *testing.T) {
	f := newLinkAttemptFixture(t, "repeat-link-owner")
	f.fake.SetAccountStatus(billingAccountA, accountsv1.BillingStatus_BILLING_STATUS_BLOCKED, f.env.Clock.Now())
	if err := f.link(f.org.OwnerClient, billingAccountA); err != nil {
		t.Fatal(err)
	}
	f.fake.SetAccountStatus(billingAccountA, accountsv1.BillingStatus_BILLING_STATUS_ALLOWED, time.Time{})
	sequence := f.fake.Status(f.org.ID).GetSequence()
	// An earlier release could checkpoint the status without applying it.
	if _, err := f.env.DB.Pool.Exec(t.Context(), "UPDATE billing_status_cursor SET after_sequence = $1 WHERE singleton", sequence); err != nil {
		t.Fatal(err)
	}
	pollBilling(t, f.env)
	if st := billingStatus(t, f.org.OwnerClient, f.org.ID); st.GetPhase() != billing.PhaseBlocked {
		t.Fatalf("GetBillingStatus before repeat link = %v, want BLOCKED", st)
	}
	if err := f.link(f.org.OwnerClient, billingAccountA); err != nil {
		t.Fatal(err)
	}
	if st := billingStatus(t, f.org.OwnerClient, f.org.ID); st.GetPhase() != billing.PhaseAllowed {
		t.Fatalf("GetBillingStatus after repeat link = %v, want ALLOWED", st)
	}
}
