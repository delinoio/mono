// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/servers/accounts-api/internal/billing"
	"github.com/delinoio/mono/servers/accounts-api/internal/testenv"
)

func unlinkAsync(env *testenv.Env, org string) func() error {
	return func() error {
		_, err := env.Service().UnlinkOrganization(ctx(), connect.NewRequest(&accountsv1.UnlinkOrganizationRequest{
			Service: ownsand, OrganizationId: org,
		}))
		return err
	}
}

func unlinkTime(t *testing.T, env *testenv.Env, org string) time.Time {
	t.Helper()
	var at time.Time
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT unlink_time FROM organization_links WHERE service = 'ownsand' AND organization_id = $1`,
		org).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at.UTC()
}

// unlinkAuditTimes returns the create times of the organization_unlinked
// audit events of the billing account.
func unlinkAuditTimes(t *testing.T, env *testenv.Env, accountID string) []time.Time {
	t.Helper()
	rows, err := env.DB.Pool.Query(ctx(), `SELECT create_time FROM billing_audit_events
		WHERE billing_account_id = $1 AND kind = 'organization_unlinked' ORDER BY audit_event_id`, accountID)
	if err != nil {
		t.Fatal(err)
	}
	times, err := pgx.CollectRows(rows, pgx.RowTo[time.Time])
	if err != nil {
		t.Fatal(err)
	}
	for i := range times {
		times[i] = times[i].UTC()
	}
	return times
}

// TestUnlinkTimeFollowsAccountLockWait starts an unlink that waits for the
// billing account lock while the clock moves past the start of a usage
// window. The unlink takes its time (and the time of its audit event) after
// it holds the lock, so the window that started during the wait still
// belongs to the account, and a later window is not attributed.
func TestUnlinkTimeFollowsAccountLockWait(t *testing.T) {
	env := testenv.New(t)
	f := newFixture(t, env)
	subscribe(t, env, f)
	org := newOrg(t)
	start := alignToWindowEnd(env) // the clock is 30 seconds before the end of the window that starts at start
	boundary := start.Add(billing.WindowLength)
	link(t, env, f.AccountID, org, f.Owner.ID, "org")
	requested := env.Clock.Now()

	// The clock reaches 30 seconds before the end of the window that starts
	// at boundary while the unlink waits for the account lock.
	lockedAt := boundary.Add(billing.WindowLength - 30*time.Second)
	err := raceMembershipChange(t, env, f.AccountID, func(tx pgx.Tx) {
		env.Clock.Advance(lockedAt.Sub(env.Clock.Now()))
	}, unlinkAsync(env, org))
	wantCode(t, err, 0)

	// The test clock advances by 1 ms on every read, so the time is not
	// exactly lockedAt; it only has to come from after the wait.
	got := unlinkTime(t, env, org)
	if got.Before(lockedAt) || !got.Before(lockedAt.Add(time.Second)) {
		t.Fatalf("unlink_time = %s, want the time after the lock wait %s (request time %s)", got, lockedAt, requested.UTC())
	}
	if audit := unlinkAuditTimes(t, env, f.AccountID); len(audit) != 1 || !audit[0].Equal(got) {
		t.Fatalf("organization_unlinked audit times = %v, want one at the unlink time %s", audit, got)
	}

	// The window that started during the wait overlaps the link, which was
	// still current at its start, and is charged to the account once.
	before := balanceOf(t, env, f.Owner.ID, f.AccountID).GetBalanceUsdMicros()
	res := reportUsage(t, env.Service(), report(org, meterEgress, boundary, 1000, usd)).GetResults()[0]
	if res.GetDuplicate() || res.GetUnattributed() || res.GetBillingAccountId() != f.AccountID || res.GetLedgerEntryId() == "" {
		t.Fatalf("result = %v, want charged to the unlinked account", res)
	}
	if got := usageLedgerEntries(t, env, f.AccountID); got != 1 {
		t.Fatalf("%d usage ledger entries, want 1", got)
	}
	if b := balanceOf(t, env, f.Owner.ID, f.AccountID).GetBalanceUsdMicros(); b != before-usd {
		t.Fatalf("balance %d, want %d", b, before-usd)
	}

	// A window that starts after the unlink overlaps no link.
	env.Clock.Advance(2 * billing.WindowLength)
	later := boundary.Add(2 * billing.WindowLength)
	res = reportUsage(t, env.Service(), report(org, meterEgress, later, 1000, usd)).GetResults()[0]
	if res.GetDuplicate() || !res.GetUnattributed() || res.GetLedgerEntryId() != "" {
		t.Fatalf("later window result = %v, want unattributed", res)
	}
	if got := usageLedgerEntries(t, env, f.AccountID); got != 1 {
		t.Fatalf("%d usage ledger entries after the later window, want 1", got)
	}
}

// TestUnlinkOfClosedLinkAfterLockWaitChangesNothing covers an unlink that
// waits for the billing account lock while another transaction closes the
// same link: it finds the row closed after the wait and keeps the unlink
// time of the first unlink and the audit log as they are.
func TestUnlinkOfClosedLinkAfterLockWaitChangesNothing(t *testing.T) {
	env := testenv.New(t)
	f := newFixture(t, env)
	org := newOrg(t)
	link(t, env, f.AccountID, org, f.Owner.ID, "org")
	closedAt := env.Clock.Now().UTC().Add(time.Minute).Truncate(time.Microsecond)
	before := readBusinessState(t, env, f.AccountID)

	err := raceMembershipChange(t, env, f.AccountID, func(tx pgx.Tx) {
		execTx(t, tx, `UPDATE organization_links SET unlink_time = $2 WHERE service = 'ownsand' AND organization_id = $1 AND unlink_time IS NULL`,
			org, closedAt)
		env.Clock.Advance(time.Hour)
	}, unlinkAsync(env, org))
	wantCode(t, err, 0)

	if got := unlinkTime(t, env, org); !got.Equal(closedAt) {
		t.Fatalf("unlink_time = %s, want the time of the first unlink %s", got, closedAt)
	}
	if after := readBusinessState(t, env, f.AccountID); after.AuditEvents != before.AuditEvents {
		t.Fatalf("audit events %d, want %d", after.AuditEvents, before.AuditEvents)
	}
	if got := unlinkAuditTimes(t, env, f.AccountID); len(got) != 0 {
		t.Fatalf("organization_unlinked audit times = %v, want none", got)
	}
}
