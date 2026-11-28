// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/accounts/accountstest"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/billing"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

func (f *linkAttemptFixture) wantOwnerUnlink(t *testing.T, want bool) {
	t.Helper()
	var exists bool
	err := f.env.DB.Pool.QueryRow(t.Context(), "SELECT EXISTS (SELECT 1 FROM billing_owner_unlinks WHERE organization_id = $1)", f.org.ID).Scan(&exists)
	if err != nil || exists != want {
		t.Fatalf("owner unlink intent exists = %t, want %t (err %v)", exists, want, err)
	}
}

func (f *linkAttemptFixture) wantCachedBillingAccount(t *testing.T, accountID string) {
	t.Helper()
	st, ok, err := billing.LoadStatus(t.Context(), f.env.DB.Pool, f.org.ID, false)
	if err != nil || !ok || st.BillingAccountID != accountID {
		t.Fatalf("cached replacement = %s, want %s (err %v)", st.BillingAccountID, accountID, err)
	}
}

func (f *linkAttemptFixture) wantAccountRequired(t *testing.T, blockedSince time.Time) {
	t.Helper()
	b := billingStatus(t, f.org.OwnerClient, f.org.ID)
	if b.GetLinked() || b.GetPhase() != billing.PhaseBlocked || b.GetReason() != ownsandv1.BillingReason_BILLING_REASON_ACCOUNT_REQUIRED ||
		!b.GetBlockedSince().AsTime().Equal(blockedSince) || !b.GetSuspendTime().AsTime().Equal(blockedSince.Add(billing.SuspendAfter)) {
		t.Fatalf("billing status = %v, want account required since %v", b, blockedSince)
	}
	_, err := f.org.OwnerClient.Device.CreateEnrollmentToken(t.Context(), connect.NewRequest(&ownsandv1.CreateEnrollmentTokenRequest{
		OrganizationId: f.org.ID, Ttl: durationpb.New(time.Hour),
	}))
	if got := billingReasonOf(t, "CreateEnrollmentToken", err); got != ownsandv1.BillingReason_BILLING_REASON_ACCOUNT_REQUIRED {
		t.Fatalf("billing gate reason = %v (err %v)", got, err)
	}
}

func TestBillingOwnerUnlinkRecovery(t *testing.T) {
	t.Parallel()
	outage := connect.NewError(connect.CodeUnavailable, errors.New("injected Accounts outage"))
	for _, fault := range []struct {
		name string
		code connect.Code
	}{
		{"before remote commit", connect.CodeUnavailable},
		{"lost reply", connect.CodeUnavailable},
		{"rejected local commit", connect.CodeInternal},
	} {
		t.Run(fault.name, func(t *testing.T) {
			f := newLinkAttemptFixture(t, "ivy")
			if err := f.link(f.org.OwnerClient, billingAccountA); err != nil {
				t.Fatal(err)
			}

			switch fault.name {
			case "before remote commit":
				f.fake.Fail(accountstest.MethodUnlinkOrganization, outage)
			case "lost reply":
				f.fake.FailReply(accountstest.MethodUnlinkOrganization, outage)
			case "rejected local commit":
				for _, stmt := range []string{
					`CREATE FUNCTION fail_owner_unlink() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected commit failure'; END $$`,
					`CREATE CONSTRAINT TRIGGER fail_owner_unlink AFTER UPDATE ON organization_billing DEFERRABLE INITIALLY DEFERRED
					  FOR EACH ROW WHEN (OLD.billing_account_id IS NOT NULL AND NEW.billing_account_id IS NULL) EXECUTE FUNCTION fail_owner_unlink()`,
				} {
					if _, err := f.env.DB.Pool.Exec(t.Context(), stmt); err != nil {
						t.Fatal(err)
					}
				}
			}

			if err := f.unlink(f.org.OwnerClient); connect.CodeOf(err) != fault.code {
				t.Fatalf("owner unlink = %v, want %v", err, fault.code)
			}
			f.wantOwnerUnlink(t, true)
			f.wantRemoteLink(t, fault.name == "before remote commit")
			f.wantLocalLink(t, true)

			f.fake.Fail(accountstest.MethodUnlinkOrganization, nil)
			f.fake.FailReply(accountstest.MethodUnlinkOrganization, nil)
			if _, err := f.env.DB.Pool.Exec(t.Context(), "DROP TRIGGER IF EXISTS fail_owner_unlink ON organization_billing"); err != nil {
				t.Fatal(err)
			}

			// A compatibility worker can read and finish the intent with writes
			// disabled, even after the original worker and request are gone.
			f.env.Clock.Advance(2 * time.Second)
			restarted := f.restart()
			var changes atomic.Int32
			restarted.OnChange(func() { changes.Add(1) })
			if fault.name == "lost reply" {
				if err := restarted.CheckLinkPresence(t.Context()); err != nil {
					t.Fatal(err)
				}
				if calls := f.fake.Calls(accountstest.MethodUnlinkOrganization); calls != 1 {
					t.Fatalf("presence scan sent an unlink, total calls = %d", calls)
				}
			} else if err := restarted.RetryPendingUnlinks(t.Context()); err != nil {
				t.Fatal(err)
			}
			f.wantOwnerUnlink(t, false)
			f.wantRemoteLink(t, false)
			f.wantAccountRequired(t, f.env.Clock.Now())
			if n := changes.Load(); n != 1 {
				t.Fatalf("committed change hooks = %d, want 1", n)
			}
		})
	}
}

func TestBillingLinkPresenceSerializesRelink(t *testing.T) {
	t.Parallel()
	f := newLinkAttemptFixture(t, "ivy", func(o *app.Options) { o.WriteOwnerUnlinks = false })
	if err := f.link(f.org.OwnerClient, billingAccountA); err != nil {
		t.Fatal(err)
	}
	f.fake.FailReply(accountstest.MethodUnlinkOrganization, connect.NewError(connect.CodeUnavailable, errors.New("injected lost reply")))
	if err := f.unlink(f.org.OwnerClient); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("legacy unlink = %v", err)
	}
	f.wantOwnerUnlink(t, false)
	f.wantRemoteLink(t, false)
	f.wantLocalLink(t, true)
	f.fake.AddAccount(billingAccountB, "Replacement", map[string]accountsv1.BillingAccountRole{"ivy": accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_OWNER})

	gate := f.fake.Hold(accountstest.MethodGetOrganizationBilling)
	defer gate.Release()
	scanned := make(chan error, 1)
	go func() { scanned <- f.restart().CheckLinkPresence(t.Context()) }()
	select {
	case <-gate.Entered:
	case <-time.After(4 * time.Second):
		t.Fatal("presence lookup did not reach Accounts")
	}

	linked := make(chan error, 1)
	go func() { linked <- f.linkWithContext(t.Context(), f.org.OwnerClient, billingAccountB) }()
	waitForLockWaiter(t, f.env, "FROM organizations", "FOR NO KEY UPDATE")
	gate.Release()
	if err := <-scanned; err != nil {
		t.Fatal(err)
	}
	if err := <-linked; err != nil {
		t.Fatal(err)
	}
	f.wantCachedBillingAccount(t, billingAccountB)
	if id, _, _ := f.fake.Link(f.org.ID); id != billingAccountB {
		t.Fatalf("remote replacement = %s", id)
	}
}

func TestBillingOwnerUnlinkSerializesRelink(t *testing.T) {
	t.Parallel()
	f := newLinkAttemptFixture(t, "ivy")
	if err := f.link(f.org.OwnerClient, billingAccountA); err != nil {
		t.Fatal(err)
	}
	f.fake.Fail(accountstest.MethodUnlinkOrganization, connect.NewError(connect.CodeUnavailable, errors.New("injected outage")))
	if err := f.unlink(f.org.OwnerClient); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("owner unlink = %v", err)
	}
	f.wantOwnerUnlink(t, true)
	f.fake.Fail(accountstest.MethodUnlinkOrganization, nil)

	// The replacement request must resolve the old intent before it checks
	// whether its requested account differs from the cached link.
	f.fake.AddAccount(billingAccountB, "Replacement", map[string]accountsv1.BillingAccountRole{"ivy": accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_OWNER})
	if err := f.link(f.org.OwnerClient, billingAccountB); err != nil {
		t.Fatalf("link with pending owner unlink = %v", err)
	}
	if err := f.restart().RetryPendingUnlinks(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.wantOwnerUnlink(t, false)
	if id, _, _ := f.fake.Link(f.org.ID); id != billingAccountB {
		t.Fatalf("remote replacement = %s", id)
	}
	f.wantCachedBillingAccount(t, billingAccountB)
}

func TestBillingOwnerUnlinkSurvivesCancellation(t *testing.T) {
	t.Parallel()
	f := newLinkAttemptFixture(t, "ivy")
	if err := f.link(f.org.OwnerClient, billingAccountA); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.fake.Fail(accountstest.MethodUnlinkOrganization, connect.NewError(connect.CodeUnavailable, errors.New("prevent mutation in the canceled call")))
	gate := f.fake.Hold(accountstest.MethodUnlinkOrganization)
	defer gate.Release()

	done := make(chan error, 1)
	go func() {
		_, err := f.org.OwnerClient.Billing.UnlinkBillingAccount(ctx, connect.NewRequest(&ownsandv1.UnlinkBillingAccountRequest{OrganizationId: f.org.ID}))
		done <- err
	}()
	select {
	case <-gate.Entered:
	case <-time.After(4 * time.Second):
		t.Fatal("unlink did not reach Accounts")
	}
	f.wantOwnerUnlink(t, true)
	cancel()
	if err := <-done; testenv.Code(err) != connect.CodeCanceled {
		t.Fatalf("canceled unlink = %v", err)
	}
	gate.Release()
	f.fake.Fail(accountstest.MethodUnlinkOrganization, nil)
	f.env.Clock.Advance(2 * time.Second)
	if err := f.restart().RetryPendingUnlinks(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.wantOwnerUnlink(t, false)
	f.wantRemoteLink(t, false)
	f.wantAccountRequired(t, f.env.Clock.Now())
}
