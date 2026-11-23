// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"testing"
	"time"

	"github.com/delinoio/mono/servers/accounts-api/internal/testenv"
)

// holdAccountRow locks the billing account row like a transaction of
// another request that holds the billing account lock, until the returned
// function ends the holder transaction.
func holdAccountRow(t *testing.T, env *testenv.Env, accountID string) (release func()) {
	t.Helper()
	tx, err := env.DB.Pool.Begin(ctx())
	if err != nil {
		t.Fatal(err)
	}
	released := false
	t.Cleanup(func() {
		if !released {
			_ = tx.Rollback(ctx())
		}
	})
	if _, err := tx.Exec(ctx(), `SELECT 1 FROM billing_accounts WHERE billing_account_id = $1 FOR UPDATE`, accountID); err != nil {
		t.Fatal(err)
	}
	return func() {
		released = true
		if err := tx.Commit(ctx()); err != nil {
			t.Fatal(err)
		}
	}
}

// waitRowLockWaiters waits until exactly n sessions of the test database
// wait for a row lock.
func waitRowLockWaiters(t *testing.T, env *testenv.Env, n int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var waiting int
		if err := env.DB.Pool.QueryRow(ctx(), `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND wait_event IN ('transactionid', 'tuple')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d sessions wait for a row lock, want %d", waiting, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// linkedThenUnlinked links the organization to the billing account, moves
// the clock by 5 seconds and unlinks it, so the link history holds one
// closed link that overlaps the window that contains the clock; the clock
// is moved a further 5 seconds afterwards.
func linkedThenUnlinked(t *testing.T, env *testenv.Env, accountID, org, userID string) {
	t.Helper()
	link(t, env, accountID, org, userID, "Org")
	env.Clock.Advance(5 * time.Second)
	unlink(t, env, org)
	env.Clock.Advance(5 * time.Second)
}

func linkedOrganizations(t *testing.T, env *testenv.Env, accountID, org string) int {
	t.Helper()
	var n int
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT count(*) FROM organization_links WHERE billing_account_id = $1 AND organization_id = $2`,
		accountID, org).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestAttributedReportHoldsOrganizationLockUntilCommit pauses an attributed
// report after it holds the billing account lock and the organization lock
// and re-read its attribution. A link of the organization to another
// billing account waits for the organization lock, so it cannot commit
// between the report's last read and its insert. After the report commits,
// its stored attribution, its result and its deduction agree on the first
// billing account, and a retry of the key returns the original result
// without a second deduction.
func TestAttributedReportHoldsOrganizationLockUntilCommit(t *testing.T) {
	env := testenv.New(t)
	f := newFixture(t, env)
	other := createAccount(t, f.Owner, "Other").GetBillingAccountId()
	org := newOrg(t)
	start := alignToWindowEnd(env)
	linkedThenUnlinked(t, env, f.AccountID, org, f.Owner.ID)
	pauseInsert(t, env, "usage_reports", pauseReportKey, false)
	release := holdKey(t, env, pauseReportKey)

	reported := reportAsync(env, report(org, meterEgress, start, 1000, usd))
	waitAdvisoryWaiters(t, env, 1) // the report holds both locks and waits in its insert
	linked := linkAsync(env, other, org, f.Owner.ID)
	waitAdvisoryWaiters(t, env, 2) // the link to the other account waits for the organization lock
	if n := linkedOrganizations(t, env, other, org); n != 0 {
		t.Fatalf("%d links to the other billing account committed while the report holds the organization lock", n)
	}
	release()

	res := waitUsage(t, reported).GetResults()[0]
	if res.GetDuplicate() || res.GetUnattributed() || res.GetBillingAccountId() != f.AccountID || res.GetLedgerEntryId() == "" {
		t.Fatalf("result = %v, want charged to the first billing account", res)
	}
	waitLink(t, linked, false)
	var storedAccount, storedEntry string
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT billing_account_id::text, ledger_entry_id::text FROM usage_reports WHERE organization_id = $1`,
		org).Scan(&storedAccount, &storedEntry); err != nil {
		t.Fatal(err)
	}
	if storedAccount != f.AccountID || storedEntry != res.GetLedgerEntryId() {
		t.Fatalf("stored attribution (%s, %s), want (%s, %s)", storedAccount, storedEntry, f.AccountID, res.GetLedgerEntryId())
	}
	if got := usageLedgerEntries(t, env, f.AccountID); got != 1 {
		t.Fatalf("%d usage ledger entries of the first account, want 1", got)
	}
	if got := usageLedgerEntries(t, env, other); got != 0 {
		t.Fatalf("%d usage ledger entries of the other account, want none", got)
	}
	balance := balanceOf(t, env, f.Owner.ID, f.AccountID).GetBalanceUsdMicros()
	if balance != -usd {
		t.Fatalf("balance of the first account %d, want %d", balance, -usd)
	}

	// A retry of the key returns the original result and deducts nothing.
	again := reportUsage(t, env.Service(), report(org, meterEgress, start, 1000, usd)).GetResults()[0]
	if !again.GetDuplicate() || again.GetUnattributed() || again.GetBillingAccountId() != f.AccountID || again.GetLedgerEntryId() != res.GetLedgerEntryId() {
		t.Fatalf("retry = %v, want the first result as duplicate", again)
	}
	if got := usageLedgerEntries(t, env, f.AccountID) + usageLedgerEntries(t, env, other); got != 1 {
		t.Fatalf("%d usage ledger entries after the retry, want 1", got)
	}
	if b := balanceOf(t, env, f.Owner.ID, f.AccountID).GetBalanceUsdMicros(); b != balance {
		t.Fatalf("balance %d after the retry, want %d", b, balance)
	}
}

// TestAttributedReportFollowsLinkThatCommitsWhileItWaitsForAccountLock holds
// the billing account lock of the first link while an attributed report,
// which read that link, waits for it. A link to another billing account
// commits meanwhile and is the latest link that overlaps the window. The
// report finds the change after it holds the locks, rolls back, retries and
// is charged to the other billing account once; the first account is not
// charged.
func TestAttributedReportFollowsLinkThatCommitsWhileItWaitsForAccountLock(t *testing.T) {
	env := testenv.New(t)
	f := newFixture(t, env)
	other := createAccount(t, f.Owner, "Other").GetBillingAccountId()
	org := newOrg(t)
	start := alignToWindowEnd(env)
	linkedThenUnlinked(t, env, f.AccountID, org, f.Owner.ID)
	release := holdAccountRow(t, env, f.AccountID)

	reported := reportAsync(env, report(org, meterEgress, start, 1000, usd))
	waitRowLockWaiters(t, env, 1) // the report read the first link and waits for its billing account lock
	link(t, env, other, org, f.Owner.ID, "Org")
	release()

	res := waitUsage(t, reported).GetResults()[0]
	if res.GetDuplicate() || res.GetUnattributed() || res.GetBillingAccountId() != other || res.GetLedgerEntryId() == "" {
		t.Fatalf("result = %v, want charged to the other billing account", res)
	}
	var storedAccount string
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT billing_account_id::text FROM usage_reports WHERE organization_id = $1`, org).Scan(&storedAccount); err != nil {
		t.Fatal(err)
	}
	if storedAccount != other {
		t.Fatalf("stored billing account %s, want %s", storedAccount, other)
	}
	if got := usageLedgerEntries(t, env, f.AccountID); got != 0 {
		t.Fatalf("%d usage ledger entries of the first account, want none", got)
	}
	if got := usageLedgerEntries(t, env, other); got != 1 {
		t.Fatalf("%d usage ledger entries of the other account, want 1", got)
	}
	if b := balanceOf(t, env, f.Owner.ID, other).GetBalanceUsdMicros(); b != -usd {
		t.Fatalf("balance of the other account %d, want %d", b, -usd)
	}
	if b := balanceOf(t, env, f.Owner.ID, f.AccountID).GetBalanceUsdMicros(); b != 0 {
		t.Fatalf("balance of the first account %d, want 0", b)
	}
}
