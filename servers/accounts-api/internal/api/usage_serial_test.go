// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/servers/accounts-api/internal/billing"
	"github.com/delinoio/mono/servers/accounts-api/internal/testenv"
)

// Keys of the test-only advisory locks that pause an insert.
const (
	pauseReportKey = 7_000_001
	pauseLinkKey   = 7_000_002
)

// pauseInsert makes every insert into table wait for the shared advisory
// lock key in a BEFORE INSERT trigger, so a test pauses the transaction of
// the insert (which already holds its own locks) by holding key exclusively.
// With fail set the insert then fails, which rolls its transaction back.
func pauseInsert(t *testing.T, env *testenv.Env, table string, key int, fail bool) {
	t.Helper()
	for _, sql := range []string{
		`CREATE OR REPLACE FUNCTION test_pause_insert() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				PERFORM pg_advisory_xact_lock_shared(TG_ARGV[0]::bigint);
				IF TG_ARGV[1] = 'fail' THEN
					RAISE EXCEPTION 'test failure after the pause';
				END IF;
				RETURN NEW;
			END $$`,
		fmt.Sprintf(`CREATE TRIGGER test_pause_%s BEFORE INSERT ON %s FOR EACH ROW EXECUTE FUNCTION test_pause_insert('%d', '%s')`,
			table, table, key, map[bool]string{true: "fail", false: "pass"}[fail]),
	} {
		if _, err := env.DB.Pool.Exec(ctx(), sql); err != nil {
			t.Fatal(err)
		}
	}
}

// holdKey holds the advisory lock key exclusively until the returned
// function commits the holder transaction.
func holdKey(t *testing.T, env *testenv.Env, key int) (release func()) {
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
	if _, err := tx.Exec(ctx(), `SELECT pg_advisory_xact_lock($1)`, key); err != nil {
		t.Fatal(err)
	}
	return func() {
		released = true
		if err := tx.Commit(ctx()); err != nil {
			t.Fatal(err)
		}
	}
}

// waitAdvisoryWaiters waits until exactly n sessions of the test database
// wait for an advisory lock.
func waitAdvisoryWaiters(t *testing.T, env *testenv.Env, n int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var waiting int
		if err := env.DB.Pool.QueryRow(ctx(), `SELECT count(*) FROM pg_locks
			WHERE locktype = 'advisory' AND NOT granted
			AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d sessions wait for an advisory lock, want %d", waiting, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// alignToWindowEnd advances the test clock to 30 seconds before the end of a
// window and returns the window start. A report of that window is accepted
// (a window may end 1 minute after the server time), and a link that is
// created now overlaps it.
func alignToWindowEnd(env *testenv.Env) time.Time {
	now := env.Clock.Now()
	end := window(now).Add(billing.WindowLength)
	if end.Sub(now) < 2*time.Minute {
		end = end.Add(billing.WindowLength)
	}
	env.Clock.Advance(end.Add(-30 * time.Second).Sub(now))
	return end.Add(-billing.WindowLength)
}

type usageOutcome struct {
	Result *accountsv1.ReportUsageResponse
	Err    error
}

func reportAsync(env *testenv.Env, reports ...*accountsv1.UsageReport) <-chan usageOutcome {
	done := make(chan usageOutcome, 1)
	go func() {
		resp, err := env.Service().ReportUsage(ctx(), connect.NewRequest(&accountsv1.ReportUsageRequest{Reports: reports}))
		out := usageOutcome{Err: err}
		if err == nil {
			out.Result = resp.Msg
		}
		done <- out
	}()
	return done
}

func linkAsync(env *testenv.Env, accountID, org, userID string) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := env.Service().LinkOrganization(ctx(), connect.NewRequest(&accountsv1.LinkOrganizationRequest{
			Service: ownsand, OrganizationId: org, BillingAccountId: accountID, UserId: userID,
		}))
		done <- err
	}()
	return done
}

func waitUsage(t *testing.T, done <-chan usageOutcome) *accountsv1.ReportUsageResponse {
	t.Helper()
	select {
	case out := <-done:
		if out.Err != nil {
			t.Fatalf("ReportUsage: %v", out.Err)
		}
		return out.Result
	case <-time.After(20 * time.Second):
		t.Fatal("ReportUsage did not finish")
		return nil
	}
}

func waitLink(t *testing.T, done <-chan error, wantErr bool) {
	t.Helper()
	select {
	case err := <-done:
		if (err != nil) != wantErr {
			t.Fatalf("LinkOrganization error = %v, want error: %v", err, wantErr)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("LinkOrganization did not finish")
	}
}

func usageLedgerEntries(t *testing.T, env *testenv.Env, accountID string) int {
	t.Helper()
	var n int
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT count(*) FROM ledger_entries WHERE billing_account_id = $1 AND kind = 'usage'`,
		accountID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestReportUsageWaitsForConcurrentFirstLink starts a first link of an
// organization that holds the organization lock and has not committed. A
// report of a window that the link overlaps waits for it, sees the committed
// link and charges its billing account once, instead of storing the report
// as unattributed.
func TestReportUsageWaitsForConcurrentFirstLink(t *testing.T) {
	env := testenv.New(t)
	f := newFixture(t, env)
	subscribe(t, env, f)
	org := newOrg(t)
	start := alignToWindowEnd(env)
	pauseInsert(t, env, "organization_links", pauseLinkKey, false)
	release := holdKey(t, env, pauseLinkKey)

	linked := linkAsync(env, f.AccountID, org, f.Owner.ID)
	waitAdvisoryWaiters(t, env, 1) // the link waits in its insert, holding the organization lock
	before := balanceOf(t, env, f.Owner.ID, f.AccountID).GetBalanceUsdMicros()
	reported := reportAsync(env, report(org, meterEgress, start, 1000, usd))
	waitAdvisoryWaiters(t, env, 2) // the report waits for the organization lock
	release()

	waitLink(t, linked, false)
	res := waitUsage(t, reported).GetResults()[0]
	if res.GetDuplicate() || res.GetUnattributed() || res.GetBillingAccountId() != f.AccountID || res.GetLedgerEntryId() == "" {
		t.Fatalf("result = %v, want charged to the new link", res)
	}
	if got := usageLedgerEntries(t, env, f.AccountID); got != 1 {
		t.Fatalf("%d usage ledger entries, want 1", got)
	}
	if b := balanceOf(t, env, f.Owner.ID, f.AccountID).GetBalanceUsdMicros(); b != before-usd {
		t.Fatalf("balance %d, want %d", b, before-usd)
	}
	// A retry returns the stored result and charges nothing again.
	again := reportUsage(t, env.Service(), report(org, meterEgress, start, 1000, usd)).GetResults()[0]
	if !again.GetDuplicate() || again.GetLedgerEntryId() != res.GetLedgerEntryId() {
		t.Fatalf("retry = %v, want the first result as duplicate", again)
	}
	if got := usageLedgerEntries(t, env, f.AccountID); got != 1 {
		t.Fatalf("%d usage ledger entries after the retry, want 1", got)
	}
}

// TestReportUsageWithoutLinkStaysUnattributed covers a report of an
// organization without any link: it is stored as unattributed, and a retry
// returns the stored row.
func TestReportUsageWithoutLinkStaysUnattributed(t *testing.T) {
	env := testenv.New(t)
	org := newOrg(t)
	start := alignToWindowEnd(env)
	res := reportUsage(t, env.Service(), report(org, meterEgress, start, 1, usd)).GetResults()[0]
	if res.GetDuplicate() || !res.GetUnattributed() || res.GetLedgerEntryId() != "" {
		t.Fatalf("result = %v, want unattributed", res)
	}
	again := reportUsage(t, env.Service(), report(org, meterEgress, start, 1, usd)).GetResults()[0]
	if !again.GetDuplicate() || !again.GetUnattributed() {
		t.Fatalf("retry = %v, want the unattributed row as duplicate", again)
	}
	if n := usageReportCount(t, env); n != 1 {
		t.Fatalf("%d usage reports, want 1", n)
	}
}

// TestFirstLinkTimeFollowsCommittedUnattributedReport pauses an unattributed
// report of a window that is about to end after it holds the organization
// lock, and starts a first link of the organization, which waits for the
// lock. The clock moves past the end of the window before the report
// commits. The link then takes its time after the report: it must not reach
// back into the window of the stored unattributed report, which a retry
// replays.
func TestFirstLinkTimeFollowsCommittedUnattributedReport(t *testing.T) {
	env := testenv.New(t)
	f := newFixture(t, env)
	subscribe(t, env, f)
	org := newOrg(t)
	start := alignToWindowEnd(env)
	end := start.Add(billing.WindowLength)
	pauseInsert(t, env, "usage_reports", pauseReportKey, false)
	release := holdKey(t, env, pauseReportKey)

	reported := reportAsync(env, report(org, meterEgress, start, 1000, usd))
	waitAdvisoryWaiters(t, env, 1) // the report holds the organization lock and waits in its insert
	linked := linkAsync(env, f.AccountID, org, f.Owner.ID)
	waitAdvisoryWaiters(t, env, 2) // the link, whose time was read already, waits for the organization lock
	env.Clock.Advance(10 * time.Minute)
	release()

	res := waitUsage(t, reported).GetResults()[0]
	if res.GetDuplicate() || !res.GetUnattributed() {
		t.Fatalf("result = %v, want unattributed", res)
	}
	waitLink(t, linked, false)
	var linkTime time.Time
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT link_time FROM organization_links WHERE service = 'ownsand' AND organization_id = $1`,
		org).Scan(&linkTime); err != nil {
		t.Fatal(err)
	}
	if linkTime.Before(end) {
		t.Fatalf("link_time %s is before the end of the reported window %s", linkTime, end)
	}
	// The retry returns the stored row and does not charge the new link.
	again := reportUsage(t, env.Service(), report(org, meterEgress, start, 1000, usd)).GetResults()[0]
	if !again.GetDuplicate() || !again.GetUnattributed() {
		t.Fatalf("retry = %v, want the unattributed row as duplicate", again)
	}
	if got := usageLedgerEntries(t, env, f.AccountID); got != 0 {
		t.Fatalf("%d usage ledger entries, want none", got)
	}
}

// TestFirstLinkStartsInsideWindowOfEarlyUnattributedReport pauses an
// unattributed report of a window that ends 30 seconds after the server time
// (a window may end up to 1 minute ahead) after it holds the organization
// lock, and starts a first link that waits for the lock. The clock moves
// only 10 seconds, which stays inside the window, before the report commits.
// The lock orders the two transactions but not the clock against the window
// end, so the link takes its time after the report committed and still
// starts before the end of the window. The retry replays the stored
// unattributed result and the later link is not charged.
func TestFirstLinkStartsInsideWindowOfEarlyUnattributedReport(t *testing.T) {
	env := testenv.New(t)
	f := newFixture(t, env)
	subscribe(t, env, f)
	org := newOrg(t)
	start := alignToWindowEnd(env) // the clock is 30 seconds before the window end
	end := start.Add(billing.WindowLength)
	pauseInsert(t, env, "usage_reports", pauseReportKey, false)
	release := holdKey(t, env, pauseReportKey)

	reported := reportAsync(env, report(org, meterEgress, start, 1000, usd))
	waitAdvisoryWaiters(t, env, 1) // the report holds the organization lock and waits in its insert
	linked := linkAsync(env, f.AccountID, org, f.Owner.ID)
	waitAdvisoryWaiters(t, env, 2) // the link waits for the organization lock
	env.Clock.Advance(10 * time.Second)
	release()

	res := waitUsage(t, reported).GetResults()[0]
	if res.GetDuplicate() || !res.GetUnattributed() {
		t.Fatalf("result = %v, want unattributed", res)
	}
	waitLink(t, linked, false)
	var linkTime time.Time
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT link_time FROM organization_links WHERE service = 'ownsand' AND organization_id = $1`,
		org).Scan(&linkTime); err != nil {
		t.Fatal(err)
	}
	if !linkTime.Before(end) {
		t.Fatalf("link_time %s is not before the end of the reported window %s", linkTime, end)
	}
	if !linkTime.After(start) {
		t.Fatalf("link_time %s is not after the start of the reported window %s", linkTime, start)
	}
	// The retry returns the stored row and does not charge the later link.
	again := reportUsage(t, env.Service(), report(org, meterEgress, start, 1000, usd)).GetResults()[0]
	if !again.GetDuplicate() || !again.GetUnattributed() || again.GetBillingAccountId() != "" || again.GetLedgerEntryId() != "" {
		t.Fatalf("retry = %v, want the unattributed row as duplicate", again)
	}
	if got := usageLedgerEntries(t, env, f.AccountID); got != 0 {
		t.Fatalf("%d usage ledger entries, want none", got)
	}
}

// TestReportUsageIgnoresFirstLinkThatRollsBack starts a first link that
// holds the organization lock and then fails. The report that waited for the
// lock finds no link and is stored as unattributed.
func TestReportUsageIgnoresFirstLinkThatRollsBack(t *testing.T) {
	env := testenv.New(t)
	f := newFixture(t, env)
	subscribe(t, env, f)
	org := newOrg(t)
	start := alignToWindowEnd(env)
	pauseInsert(t, env, "organization_links", pauseLinkKey, true)
	release := holdKey(t, env, pauseLinkKey)

	linked := linkAsync(env, f.AccountID, org, f.Owner.ID)
	waitAdvisoryWaiters(t, env, 1)
	reported := reportAsync(env, report(org, meterEgress, start, 1000, usd))
	waitAdvisoryWaiters(t, env, 2)
	release()

	waitLink(t, linked, true)
	res := waitUsage(t, reported).GetResults()[0]
	if res.GetDuplicate() || !res.GetUnattributed() {
		t.Fatalf("result = %v, want unattributed", res)
	}
	var links int
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT count(*) FROM organization_links WHERE organization_id = $1`, org).Scan(&links); err != nil || links != 0 {
		t.Fatalf("%d links after the failed link, %v", links, err)
	}
	if got := usageLedgerEntries(t, env, f.AccountID); got != 0 {
		t.Fatalf("%d usage ledger entries, want none", got)
	}
	again := reportUsage(t, env.Service(), report(org, meterEgress, start, 1000, usd)).GetResults()[0]
	if !again.GetDuplicate() || !again.GetUnattributed() {
		t.Fatalf("retry = %v, want the unattributed row as duplicate", again)
	}
}
