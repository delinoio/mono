// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/go/uuidv7"
	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/protos/delino/accounts/v1/accountsv1connect"
	"github.com/delinoio/mono/servers/accounts-api/internal/auth"
	"github.com/delinoio/mono/servers/accounts-api/internal/auth/authtest"
	"github.com/delinoio/mono/servers/accounts-api/internal/billing"
	"github.com/delinoio/mono/servers/accounts-api/internal/email"
	"github.com/delinoio/mono/servers/accounts-api/internal/testenv"
)

const (
	ownsand      = accountsv1.ServiceId_SERVICE_ID_OWNSAND
	meterEgress  = accountsv1.Meter_METER_OWNSAND_RELAY_EGRESS
	meterStorage = accountsv1.Meter_METER_OWNSAND_R2_STORAGE
	meterClassA  = accountsv1.Meter_METER_OWNSAND_CLASS_A_RPC
	meterClassB  = accountsv1.Meter_METER_OWNSAND_CLASS_B_RPC
	allowed      = accountsv1.BillingStatus_BILLING_STATUS_ALLOWED
	blocked      = accountsv1.BillingStatus_BILLING_STATUS_BLOCKED
)

func newOrg(t *testing.T) string {
	t.Helper()
	id, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// window returns the start of the 5-minute window that contains t.
func window(t time.Time) time.Time { return t.UTC().Truncate(billing.WindowLength) }

func report(org string, meter accountsv1.Meter, start time.Time, quantity, amount int64) *accountsv1.UsageReport {
	return &accountsv1.UsageReport{Service: ownsand, OrganizationId: org, Meter: meter, WindowStart: timestamppb.New(start),
		Quantity: quantity, AmountUsdMicros: amount}
}

func reportUsage(t *testing.T, c accountsv1connect.UsageServiceClient, reports ...*accountsv1.UsageReport) *accountsv1.ReportUsageResponse {
	t.Helper()
	resp, err := c.ReportUsage(ctx(), connect.NewRequest(&accountsv1.ReportUsageRequest{Reports: reports}))
	if err != nil {
		t.Fatalf("ReportUsage: %v", err)
	}
	return resp.Msg
}

func link(t *testing.T, env *testenv.Env, accountID, org, userID, name string) *accountsv1.LinkOrganizationResponse {
	t.Helper()
	resp, err := env.Service().LinkOrganization(ctx(), connect.NewRequest(&accountsv1.LinkOrganizationRequest{
		Service: ownsand, OrganizationId: org, BillingAccountId: accountID, UserId: userID, OrganizationName: name,
	}))
	if err != nil {
		t.Fatalf("LinkOrganization: %v", err)
	}
	return resp.Msg
}

func unlink(t *testing.T, env *testenv.Env, org string) {
	t.Helper()
	if _, err := env.Service().UnlinkOrganization(ctx(), connect.NewRequest(&accountsv1.UnlinkOrganizationRequest{
		Service: ownsand, OrganizationId: org,
	})); err != nil {
		t.Fatalf("UnlinkOrganization: %v", err)
	}
}

func poll(t *testing.T, env *testenv.Env, after int64, size int32) *accountsv1.ListBillingStatusChangesResponse {
	t.Helper()
	resp, err := env.Service().ListBillingStatusChanges(ctx(), connect.NewRequest(&accountsv1.ListBillingStatusChangesRequest{
		Service: ownsand, AfterSequence: after, PageSize: size,
	}))
	if err != nil {
		t.Fatalf("ListBillingStatusChanges: %v", err)
	}
	return resp.Msg
}

func usageReportCount(t *testing.T, env *testenv.Env) int {
	t.Helper()
	var n int
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT count(*) FROM usage_reports`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestUsageServiceAuthorization(t *testing.T) {
	env := testenv.New(t)
	call := func(c accountsv1connect.UsageServiceClient) error {
		_, err := c.ListBillingStatusChanges(ctx(), connect.NewRequest(&accountsv1.ListBillingStatusChangesRequest{Service: ownsand}))
		return err
	}
	// Missing, invalid and ID tokens are unauthenticated.
	wantCode(t, call(env.Usage("")), connect.CodeUnauthenticated)
	wantCode(t, call(env.Usage("not-a-token")), connect.CodeUnauthenticated)
	wantCode(t, call(env.Usage(env.Issuer.IDToken(t, "user-1"))), connect.CodeUnauthenticated)
	wantCode(t, call(env.Usage(env.Issuer.Sign(t, authtest.Claims{Subject: testenv.UsageClientID, ClientID: testenv.UsageClientID,
		Audience: "https://ownsand.api.delino.io", Scopes: []string{auth.ScopeReportUsage}}))), connect.CodeUnauthenticated)
	wantCode(t, call(env.Usage(env.Issuer.Sign(t, authtest.Claims{Subject: testenv.UsageClientID, ClientID: testenv.UsageClientID,
		Scopes: []string{auth.ScopeReportUsage}, Expiry: time.Now().Add(-time.Hour)}))), connect.CodeUnauthenticated)
	// A user token, also one that carries the usage scope, is
	// permission_denied.
	wantCode(t, call(env.Usage(env.Issuer.UserToken(t, "user-1"))), connect.CodePermissionDenied)
	wantCode(t, call(env.Usage(env.Issuer.Sign(t, authtest.Claims{Subject: "user-1", ClientID: "web-app",
		Scopes: []string{auth.ScopeReportUsage}}))), connect.CodePermissionDenied)
	// A machine-to-machine token without the scope is permission_denied.
	wantCode(t, call(env.Usage(env.Issuer.MachineToken(t, testenv.UsageClientID))), connect.CodePermissionDenied)
	wantCode(t, call(env.Usage(env.Issuer.MachineToken(t, testenv.UsageClientID, auth.ScopeBilling))), connect.CodePermissionDenied)
	// A client that is not mapped to the service is permission_denied.
	wantCode(t, call(env.Usage(env.Issuer.MachineToken(t, testenv.UnmappedClientID, auth.ScopeReportUsage))), connect.CodePermissionDenied)
	_, err := env.Usage(env.Issuer.MachineToken(t, testenv.UnmappedClientID, auth.ScopeReportUsage)).ListManageableBillingAccounts(ctx(),
		connect.NewRequest(&accountsv1.ListManageableBillingAccountsRequest{UserId: "user-1"}))
	wantCode(t, err, connect.CodePermissionDenied)
	wantCode(t, call(env.Service()), 0)

	logs := env.Logs.String()
	for _, reason := range []string{"missing_token", "token_type", "claims", "token_kind", "missing_scope"} {
		if !strings.Contains(logs, `"reason":"`+reason+`"`) {
			t.Errorf("no auth_rejected record with reason %s", reason)
		}
	}
	if !strings.Contains(logs, `"msg":"usage_client_not_mapped","client_id":"`+testenv.UnmappedClientID+`"`) {
		t.Error("no usage_client_not_mapped record")
	}
}

func TestBillingUsageServiceRejectsMachineTokens(t *testing.T) {
	env := testenv.New(t)
	f := newFixture(t, env)
	c := env.BillingUsageClient(env.Issuer.MachineToken(t, testenv.UsageClientID, auth.ScopeBilling, auth.ScopeReportUsage))
	_, err := c.ListLinkedOrganizations(ctx(), connect.NewRequest(&accountsv1.ListLinkedOrganizationsRequest{BillingAccountId: f.AccountID}))
	wantCode(t, err, connect.CodePermissionDenied)
}

func TestReportUsageValidation(t *testing.T) {
	env := testenv.New(t)
	c := env.Service()
	org := newOrg(t)
	now := time.Now().UTC()
	good := report(org, meterEgress, window(now).Add(-time.Hour), 10, 5)
	future := window(now).Add(10 * time.Minute)
	for name, bad := range map[string]*accountsv1.UsageReport{
		"no service":       {OrganizationId: org, Meter: meterEgress, WindowStart: good.WindowStart},
		"unknown service":  {Service: 99, OrganizationId: org, Meter: meterEgress, WindowStart: good.WindowStart},
		"empty org":        report("", meterEgress, window(now).Add(-time.Hour), 1, 1),
		"long org":         report(strings.Repeat("a", 129), meterEgress, window(now).Add(-time.Hour), 1, 1),
		"non-ascii org":    report("org-é", meterEgress, window(now).Add(-time.Hour), 1, 1),
		"no meter":         report(org, accountsv1.Meter_METER_UNSPECIFIED, window(now).Add(-time.Hour), 1, 1),
		"no window":        {Service: ownsand, OrganizationId: org, Meter: meterEgress},
		"unaligned":        report(org, meterEgress, window(now).Add(-time.Hour+time.Minute), 1, 1),
		"nanos":            report(org, meterEgress, window(now).Add(-time.Hour+time.Nanosecond), 1, 1),
		"future":           report(org, meterEgress, future, 1, 1),
		"negative qty":     report(org, meterEgress, window(now).Add(-time.Hour), -1, 1),
		"negative amount":  report(org, meterEgress, window(now).Add(-time.Hour), 1, -1),
		"nil report entry": nil,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := c.ReportUsage(ctx(), connect.NewRequest(&accountsv1.ReportUsageRequest{Reports: []*accountsv1.UsageReport{good, bad}}))
			wantCode(t, err, connect.CodeInvalidArgument)
			if !strings.Contains(err.Error(), "reports[1]") {
				t.Fatalf("error %v does not name the index", err)
			}
		})
	}
	// The valid report of the failed requests was not applied.
	if n := usageReportCount(t, env); n != 0 {
		t.Fatalf("%d usage reports stored after invalid requests", n)
	}
	_, err := c.ReportUsage(ctx(), connect.NewRequest(&accountsv1.ReportUsageRequest{}))
	wantCode(t, err, connect.CodeInvalidArgument)
	many := make([]*accountsv1.UsageReport, 501)
	for i := range many {
		many[i] = report(org, meterEgress, window(now).Add(-time.Duration(i+1)*billing.WindowLength), 1, 1)
	}
	_, err = c.ReportUsage(ctx(), connect.NewRequest(&accountsv1.ReportUsageRequest{Reports: many}))
	wantCode(t, err, connect.CodeInvalidArgument)
	// A window that ends within 1 minute after the server time and an old
	// window are accepted; 500 reports are accepted.
	current := window(now.Add(time.Minute))
	if current.Add(billing.WindowLength).After(now.Add(time.Minute)) {
		current = current.Add(-billing.WindowLength)
	}
	reportUsage(t, c, report(org, meterEgress, current, 1, 0), report(org, meterEgress, time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC), 1, 0))
	reportUsage(t, c, many[:500]...)
}

// TestZeroAmountReportExpiringGrantQueuesLowBalance reports usage of zero
// after the only grant of the billing account is due. The report expires the
// grant inline, which takes the balance below the alert threshold, so the
// low-balance email is queued and sent once.
func TestZeroAmountReportExpiringGrantQueuesLowBalance(t *testing.T) {
	env := testenv.New(t)
	f := newFixture(t, env)
	subscribe(t, env, f) // a 10 USD grant, equal to the default alert threshold
	org := newOrg(t)
	link(t, env, f.AccountID, org, f.Owner.ID, "Acme")
	lowBalanceQueued := func() int {
		var n int
		if err := env.DB.Pool.QueryRow(ctx(), `SELECT count(*) FROM billing_notifications WHERE kind = $1`,
			string(billing.NotifyLowBalance)).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := lowBalanceQueued(); n != 0 {
		t.Fatalf("%d low-balance notifications before the expiry", n)
	}

	env.Clock.Advance(time.Hour + (billing.GrantExpireTime(env.Clock.Now()).Sub(env.Clock.Now())))
	w := window(env.Clock.Now()).Add(-billing.WindowLength)
	reportUsage(t, env.Service(), report(org, meterEgress, w, 1, 0))

	if b := balanceOf(t, env, f.Owner.ID, f.AccountID).GetBalanceUsdMicros(); b != 0 {
		t.Fatalf("balance %d after the expiry", b)
	}
	if kinds := ledgerKinds(t, env, f.Owner.ID, f.AccountID); strings.Join(kinds, " ") != "USAGE EXPIRY SUBSCRIPTION_CREDIT" {
		t.Fatalf("ledger = %v", kinds)
	}
	if n := lowBalanceQueued(); n != 1 {
		t.Fatalf("%d low-balance notifications after the expiry, want 1", n)
	}
	if err := env.Billing.DeliverNotifications(ctx()); err != nil {
		t.Fatal(err)
	}
	var sent int
	for _, m := range env.Email.Messages() {
		if m.Kind == email.KindLowBalance {
			sent++
		}
	}
	if sent == 0 {
		t.Fatal("no low-balance email was sent")
	}
}

func TestReportUsageIdempotency(t *testing.T) {
	env := testenv.New(t)
	f := newFixture(t, env)
	subscribe(t, env, f) // $10 of credit and an ACTIVE subscription
	org := newOrg(t)
	link(t, env, f.AccountID, org, f.Owner.ID, "Acme")
	// Windows after the link time overlap the link.
	env.Clock.Advance(15 * time.Minute)
	w := window(env.Clock.Now()).Add(-billing.WindowLength)
	r := report(org, meterEgress, w, 1000, 3*usd)

	first := reportUsage(t, env.Service(), r)
	res := first.GetResults()[0]
	if res.GetDuplicate() || res.GetUnattributed() || res.GetBillingAccountId() != f.AccountID || res.GetLedgerEntryId() == "" ||
		res.GetOrganizationId() != org || res.GetMeter() != meterEgress || !res.GetWindowStart().AsTime().Equal(w) {
		t.Fatalf("first result = %v", res)
	}
	if len(first.GetStatuses()) != 1 || first.GetStatuses()[0].GetStatus() != allowed {
		t.Fatalf("statuses = %v", first.GetStatuses())
	}
	// The same key again, also twice in one request and with other values,
	// returns the first result and deducts nothing again.
	other := report(org, meterEgress, w, 99, 9*usd)
	again := reportUsage(t, env.Service(), r, other)
	for i, dup := range again.GetResults() {
		if !dup.GetDuplicate() || dup.GetBillingAccountId() != f.AccountID || dup.GetLedgerEntryId() != res.GetLedgerEntryId() {
			t.Fatalf("result %d = %v, want the first result as duplicate", i, dup)
		}
	}
	if b := balanceOf(t, env, f.Owner.ID, f.AccountID).GetBalanceUsdMicros(); b != 7*usd {
		t.Fatalf("balance %d, want one deduction", b)
	}
	if kinds := ledgerKinds(t, env, f.Owner.ID, f.AccountID); strings.Join(kinds, " ") != "USAGE SUBSCRIPTION_CREDIT" {
		t.Fatalf("ledger = %v", kinds)
	}
	if !strings.Contains(env.Logs.String(), `"msg":"billing_usage_report_mismatch"`) {
		t.Error("no mismatch record for the repeated key with other values")
	}
	// Within one new request, the second report of a key is a duplicate.
	w2 := w.Add(-billing.WindowLength)
	both := reportUsage(t, env.Service(), report(org, meterEgress, w2, 1, usd), report(org, meterEgress, w2, 1, usd))
	if both.GetResults()[0].GetDuplicate() || !both.GetResults()[1].GetDuplicate() ||
		both.GetResults()[0].GetLedgerEntryId() != both.GetResults()[1].GetLedgerEntryId() {
		t.Fatalf("results = %v", both.GetResults())
	}
	if b := balanceOf(t, env, f.Owner.ID, f.AccountID).GetBalanceUsdMicros(); b != 6*usd {
		t.Fatalf("balance %d", b)
	}
	// The USAGE ledger entry carries the usage fields.
	entries, err := env.Credit(f.Owner.ID).ListLedgerEntries(ctx(), connect.NewRequest(&accountsv1.ListLedgerEntriesRequest{BillingAccountId: f.AccountID}))
	if err != nil {
		t.Fatal(err)
	}
	e := entries.Msg.GetLedgerEntries()[1]
	if e.GetLedgerEntryId() != res.GetLedgerEntryId() || e.GetService() != "ownsand" || e.GetOrganizationId() != org ||
		e.GetMeter() != "ownsand.relay_egress" || !e.GetWindowStart().AsTime().Equal(w) || e.GetQuantity() != 1000 ||
		e.GetAmountUsdMicros() != -3*usd {
		t.Fatalf("usage ledger entry = %v", e)
	}
}

// TestReportUsageReplaySuspendsMoreThanOneBatchOfAccounts replays the
// reports of 101 organizations on 101 distinct unpaid billing accounts after
// every suspension time passed. The test server runs no worker, so the
// replay itself records the suspensions, in more than one batch of accounts.
func TestReportUsageReplaySuspendsMoreThanOneBatchOfAccounts(t *testing.T) {
	const n = 101
	env := testenv.New(t)
	owner := newPerson(env, "owner")
	accounts := make([]string, n)
	orgs := make([]string, n)
	for i := range orgs {
		accounts[i] = createAccount(t, owner, fmt.Sprintf("Unpaid %d", i)).GetBillingAccountId()
		orgs[i] = newOrg(t)
		// An account without a subscription blocks its link at once.
		if st := link(t, env, accounts[i], orgs[i], owner.ID, fmt.Sprintf("Org %d", i)).GetStatus(); st.GetStatus() != blocked {
			t.Fatalf("status of organization %d = %v, want BLOCKED", i, st)
		}
	}
	env.Clock.Advance(15 * time.Minute)
	w := window(env.Clock.Now()).Add(-billing.WindowLength)
	reports := make([]*accountsv1.UsageReport, n)
	for i, org := range orgs {
		reports[i] = report(org, meterEgress, w, 1, usd)
	}
	first := reportUsage(t, env.Service(), reports...)
	if len(first.GetStatuses()) != n {
		t.Fatalf("first report returned %d statuses, want %d", len(first.GetStatuses()), n)
	}
	for _, res := range first.GetResults() {
		if res.GetDuplicate() || res.GetUnattributed() {
			t.Fatalf("first result = %v", res)
		}
	}
	for _, st := range first.GetStatuses() {
		if st.GetStatus() != blocked {
			t.Fatalf("status before the suspension time = %v, want BLOCKED", st)
		}
	}

	env.Clock.Advance(billing.SuspendAfter)
	again := reportUsage(t, env.Service(), reports...)
	for i, res := range again.GetResults() {
		if !res.GetDuplicate() || res.GetLedgerEntryId() != first.GetResults()[i].GetLedgerEntryId() {
			t.Fatalf("replayed result %d = %v, want the first result as duplicate", i, res)
		}
	}
	if len(again.GetStatuses()) != n {
		t.Fatalf("replay returned %d statuses, want %d", len(again.GetStatuses()), n)
	}
	for _, st := range again.GetStatuses() {
		if st.GetStatus() != accountsv1.BillingStatus_BILLING_STATUS_SUSPENDED {
			t.Fatalf("status after the suspension time = %v, want SUSPENDED", st)
		}
	}
	// The replay deducted no credit again: each account has its one usage entry.
	for _, acct := range accounts {
		if b := balanceOf(t, env, owner.ID, acct).GetBalanceUsdMicros(); b != -usd {
			t.Fatalf("balance of %s = %d, want one deduction", acct, b)
		}
	}
	var blockedLinks int
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT count(*) FROM organization_links WHERE billing_status = 'blocked'`).Scan(&blockedLinks); err != nil || blockedLinks != 0 {
		t.Fatalf("%d links stay blocked after the replay, %v", blockedLinks, err)
	}
}

func TestReportUsageConcurrentForOneBillingAccount(t *testing.T) {
	env := testenv.New(t)
	f := newFixture(t, env)
	subscribe(t, env, f)
	orgs := []string{newOrg(t), newOrg(t), newOrg(t)}
	for _, org := range orgs {
		link(t, env, f.AccountID, org, f.Admin.ID, "")
	}
	env.Clock.Advance(2 * time.Hour)
	base := window(env.Clock.Now()).Add(-time.Hour)
	const workers = 12
	var wg sync.WaitGroup
	errs := make(chan error, workers*2)
	for i := range workers {
		for range 2 { // every request is sent twice at the same time
			wg.Add(1)
			go func() {
				defer wg.Done()
				var reports []*accountsv1.UsageReport
				for j, org := range orgs {
					reports = append(reports, report(org, meterEgress, base.Add(time.Duration(i)*billing.WindowLength), int64(j+1), 250_000))
				}
				_, err := env.Service().ReportUsage(ctx(), connect.NewRequest(&accountsv1.ReportUsageRequest{Reports: reports}))
				errs <- err
			}()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("ReportUsage: %v", err)
		}
	}
	// 12 windows of 3 organizations at $0.25 each: $9 from $10, once.
	if b := balanceOf(t, env, f.Owner.ID, f.AccountID).GetBalanceUsdMicros(); b != usd {
		t.Fatalf("balance %d, want %d", b, usd)
	}
	var usage, ledgerSum int64
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT count(*), COALESCE(SUM(amount_usd_micros), 0) FROM ledger_entries
		WHERE billing_account_id = $1 AND kind = 'usage'`, f.AccountID).Scan(&usage, &ledgerSum); err != nil {
		t.Fatal(err)
	}
	if usage != workers*int64(len(orgs)) || ledgerSum != -9*usd {
		t.Fatalf("%d usage entries with %d, want %d with %d", usage, ledgerSum, workers*len(orgs), -9*usd)
	}

	// A burst that drains the balance below zero blocks every organization,
	// and the balance goes negative.
	var burst []*accountsv1.UsageReport
	for _, org := range orgs {
		burst = append(burst, report(org, meterClassA, base, 1, 2*usd))
	}
	resp := reportUsage(t, env.Service(), burst...)
	if len(resp.GetStatuses()) != 3 {
		t.Fatalf("statuses = %v", resp.GetStatuses())
	}
	for _, st := range resp.GetStatuses() {
		if st.GetStatus() != blocked || st.GetReason() != accountsv1.BillingStatusReason_BILLING_STATUS_REASON_NON_POSITIVE_BALANCE ||
			st.GetBlockedSince() == nil || !st.GetSuspendTime().AsTime().Equal(st.GetBlockedSince().AsTime().Add(billing.SuspendAfter)) {
			t.Fatalf("status = %v", st)
		}
	}
	if b := balanceOf(t, env, f.Owner.ID, f.AccountID).GetBalanceUsdMicros(); b != -5*usd {
		t.Fatalf("balance %d, want -5 USD", b)
	}
}

func TestReportUsageAttributionThroughLinkHistory(t *testing.T) {
	env := testenv.New(t)
	a := newFixture(t, env)
	b := newFixture(t, env)
	subscribe(t, env, a)
	subscribe(t, env, b)
	org := newOrg(t)
	t0 := env.Clock.Now()
	before := window(t0).Add(-2 * billing.WindowLength)
	link(t, env, a.AccountID, org, a.Owner.ID, "")
	env.Clock.Advance(time.Hour)
	unlink(t, env, org)
	env.Clock.Advance(time.Hour)
	link(t, env, b.AccountID, org, b.Owner.ID, "")
	env.Clock.Advance(time.Hour)
	var unlinkedAt, relinkedAt time.Time
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT (SELECT unlink_time FROM organization_links WHERE organization_id = $1 AND billing_account_id = $2),
		(SELECT link_time FROM organization_links WHERE organization_id = $1 AND billing_account_id = $3)`,
		org, a.AccountID, b.AccountID).Scan(&unlinkedAt, &relinkedAt); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		start   time.Time
		account string
	}{
		{before, ""}, // before the first link
		{window(t0.Add(30 * time.Minute)), a.AccountID}, // while linked to A
		{window(unlinkedAt), a.AccountID},               // the window of the unlink still overlapped A
		{window(unlinkedAt.Add(30 * time.Minute)), ""},  // between the links
		{window(relinkedAt), b.AccountID},               // the window of the relink overlapped B
		{window(relinkedAt.Add(30 * time.Minute)), b.AccountID},
	}
	var reports []*accountsv1.UsageReport
	for _, c := range cases {
		reports = append(reports, report(org, meterStorage, c.start, 7, 100_000))
	}
	resp := reportUsage(t, env.Service(), reports...)
	for i, c := range cases {
		res := resp.GetResults()[i]
		if res.GetBillingAccountId() != c.account || res.GetUnattributed() != (c.account == "") ||
			(res.GetLedgerEntryId() == "") != (c.account == "") {
			t.Errorf("report %d (%v) = %v, want billing account %q", i, c.start, res, c.account)
		}
	}
	// Only the current link reports a status.
	if len(resp.GetStatuses()) != 1 || resp.GetStatuses()[0].GetBillingAccountId() != b.AccountID {
		t.Fatalf("statuses = %v", resp.GetStatuses())
	}
	// A window that overlaps both links goes to the overlapping link with the latest link time.
	org2 := newOrg(t)
	link(t, env, a.AccountID, org2, a.Owner.ID, "")
	unlink(t, env, org2)
	relink := env.Clock.Now()
	link(t, env, b.AccountID, org2, b.Owner.ID, "")
	env.Clock.Advance(billing.WindowLength)
	both := reportUsage(t, env.Service(), report(org2, meterStorage, window(relink), 1, 1))
	if got := both.GetResults()[0].GetBillingAccountId(); got != b.AccountID {
		t.Fatalf("a window that overlapped both links went to %q, want %q", got, b.AccountID)
	}

	// The organization usage counts every report of the month; the summary
	// of each billing account counts only its own.
	billingResp, err := env.Service().GetOrganizationBilling(ctx(), connect.NewRequest(&accountsv1.GetOrganizationBillingRequest{
		Service: ownsand, OrganizationId: org,
	}))
	if err != nil {
		t.Fatal(err)
	}
	monthStart := billingResp.Msg.GetMonthStart().AsTime()
	var wantQty int64
	for _, c := range cases {
		if !c.start.Before(monthStart) {
			wantQty += 7
		}
	}
	var gotQty int64
	for _, mu := range billingResp.Msg.GetMeterUsages() {
		if mu.GetMeter() == meterStorage {
			gotQty = mu.GetQuantity()
		}
	}
	if gotQty != wantQty {
		t.Fatalf("storage quantity %d, want %d", gotQty, wantQty)
	}
}

// startOfNextWindow advances the clock to 30 seconds into the next 5-minute
// window and returns the start of that window.
func startOfNextWindow(env *testenv.Env) time.Time {
	now := env.Clock.Now()
	start := window(now).Add(billing.WindowLength)
	env.Clock.Advance(start.Add(30 * time.Second).Sub(now))
	return start
}

// emptyLink makes the link of the organization an empty interval, as a link
// that was unlinked at the instant it started. It changes only the current
// link.
func emptyLink(t *testing.T, env *testenv.Env, org string) {
	t.Helper()
	tag, err := env.DB.Pool.Exec(ctx(), `UPDATE organization_links SET unlink_time = link_time WHERE organization_id = $1 AND unlink_time IS NULL`, org)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("empty the link: %v (%d rows)", err, tag.RowsAffected())
	}
}

func TestReportUsageIgnoresEmptyLinkInterval(t *testing.T) {
	env := testenv.New(t)
	a := newFixture(t, env)
	b := newFixture(t, env)
	subscribe(t, env, a)
	subscribe(t, env, b)

	// An empty link that is the latest one inside the window does not take
	// the report from the older link that overlapped the window.
	org := newOrg(t)
	start := startOfNextWindow(env)
	link(t, env, a.AccountID, org, a.Owner.ID, "")
	env.Clock.Advance(30 * time.Second)
	unlink(t, env, org)
	link(t, env, b.AccountID, org, b.Owner.ID, "")
	emptyLink(t, env, org)
	env.Clock.Advance(4 * time.Minute) // the window may end at most a minute after the server time
	res := reportUsage(t, env.Service(), report(org, meterStorage, start, 3, 5_000)).GetResults()[0]
	if res.GetUnattributed() || res.GetBillingAccountId() != a.AccountID {
		t.Fatalf("report with an empty newer link = %v, want billing account %q", res, a.AccountID)
	}

	// Without another overlapping link the report is unattributed.
	org2 := newOrg(t)
	start2 := startOfNextWindow(env)
	link(t, env, a.AccountID, org2, a.Owner.ID, "")
	emptyLink(t, env, org2)
	env.Clock.Advance(4 * time.Minute)
	res2 := reportUsage(t, env.Service(), report(org2, meterStorage, start2, 3, 5_000)).GetResults()[0]
	if !res2.GetUnattributed() || res2.GetBillingAccountId() != "" || res2.GetLedgerEntryId() != "" {
		t.Fatalf("report with only an empty link = %v, want unattributed", res2)
	}
}

func TestBillingStatusPollCursor(t *testing.T) {
	env := testenv.New(t)
	f := newFixture(t, env)
	subID := subscribe(t, env, f)
	org1, org2 := newOrg(t), newOrg(t)
	l1 := link(t, env, f.AccountID, org1, f.Owner.ID, "One")
	l2 := link(t, env, f.AccountID, org2, f.Owner.ID, "Two")
	if l1.GetStatus().GetStatus() != allowed || l1.GetStatus().GetSequence() <= 0 || l2.GetStatus().GetSequence() <= l1.GetStatus().GetSequence() ||
		l1.GetBillingAccountName() != "Fixture" {
		t.Fatalf("link responses = %v, %v", l1, l2)
	}

	all := poll(t, env, 0, 0)
	if len(all.GetStatuses()) != 2 || all.GetStatuses()[0].GetOrganizationId() != org1 || all.GetStatuses()[1].GetOrganizationId() != org2 ||
		all.GetNextSequence() != l2.GetStatus().GetSequence() {
		t.Fatalf("poll from 0 = %v", all)
	}
	// Paging with page_size 1.
	page := poll(t, env, 0, 1)
	if len(page.GetStatuses()) != 1 || page.GetNextSequence() != l1.GetStatus().GetSequence() {
		t.Fatalf("first page = %v", page)
	}
	page = poll(t, env, page.GetNextSequence(), 1)
	if len(page.GetStatuses()) != 1 || page.GetStatuses()[0].GetOrganizationId() != org2 {
		t.Fatalf("second page = %v", page)
	}
	cursor := all.GetNextSequence()
	env.Clock.Advance(10 * time.Minute)
	if empty := poll(t, env, cursor, 0); len(empty.GetStatuses()) != 0 || empty.GetNextSequence() != cursor {
		t.Fatalf("poll at the head = %v", empty)
	}

	// Draining the balance blocks both, and the cancellation of the
	// subscription changes the reason: two changes of each organization
	// give one entry each, with its latest status.
	reportUsage(t, env.Service(), report(org1, meterEgress, window(env.Clock.Now()).Add(-billing.WindowLength), 1, 12*usd))
	if code := env.Webhook("subscription.canceled", map[string]any{"id": subID, "status": "canceled", "updated_at": env.Clock.Now()}); code != 200 {
		t.Fatalf("webhook status %d", code)
	}
	changes := poll(t, env, cursor, 0)
	if len(changes.GetStatuses()) != 2 || changes.GetNextSequence() != cursor+4 {
		t.Fatalf("changes = %v", changes)
	}
	var org1Seq int64
	for _, st := range changes.GetStatuses() {
		if st.GetStatus() != blocked || st.GetSequence() <= cursor ||
			st.GetReason() != accountsv1.BillingStatusReason_BILLING_STATUS_REASON_NO_ACTIVE_SUBSCRIPTION {
			t.Fatalf("change = %v", st)
		}
		if st.GetOrganizationId() == org1 {
			org1Seq = st.GetSequence()
		}
	}
	// A relink to the same billing account changes only the name.
	again := link(t, env, f.AccountID, org1, f.Admin.ID, "One renamed")
	if again.GetStatus().GetSequence() != org1Seq || again.GetBillingAccountName() != "Fixture" {
		t.Fatalf("relink changed the status: %v", again)
	}
	if n := poll(t, env, changes.GetNextSequence(), 0); len(n.GetStatuses()) != 0 {
		t.Fatalf("relink added a status change: %v", n)
	}
	// An unlink adds no entry, and the organization leaves the full list.
	unlink(t, env, org2)
	if n := poll(t, env, changes.GetNextSequence(), 0); len(n.GetStatuses()) != 0 {
		t.Fatalf("unlink added a status change: %v", n)
	}
	if n := poll(t, env, 0, 0); len(n.GetStatuses()) != 1 || n.GetStatuses()[0].GetOrganizationId() != org1 {
		t.Fatalf("poll from 0 after the unlink = %v", n)
	}
	// After 3 days the poll reports the suspension without a usage report
	// (the test server runs no worker).
	env.Clock.Advance(billing.SuspendAfter)
	suspended := poll(t, env, changes.GetNextSequence(), 0)
	if len(suspended.GetStatuses()) != 1 || suspended.GetStatuses()[0].GetStatus() != accountsv1.BillingStatus_BILLING_STATUS_SUSPENDED ||
		suspended.GetStatuses()[0].GetOrganizationId() != org1 {
		t.Fatalf("poll after 3 days = %v", suspended)
	}
	_, err := env.Service().ListBillingStatusChanges(ctx(), connect.NewRequest(&accountsv1.ListBillingStatusChangesRequest{Service: ownsand, AfterSequence: -1}))
	wantCode(t, err, connect.CodeInvalidArgument)
	_, err = env.Service().ListBillingStatusChanges(ctx(), connect.NewRequest(&accountsv1.ListBillingStatusChangesRequest{}))
	wantCode(t, err, connect.CodeInvalidArgument)
	_, err = env.Service().ListBillingStatusChanges(ctx(), connect.NewRequest(&accountsv1.ListBillingStatusChangesRequest{Service: ownsand, PageSize: -1}))
	wantCode(t, err, connect.CodeInvalidArgument)
}

func TestLinkOrganizationRoles(t *testing.T) {
	env := testenv.New(t)
	f := newFixture(t, env)
	other := newFixture(t, env)
	try := func(accountID, userID, org string) error {
		_, err := env.Service().LinkOrganization(ctx(), connect.NewRequest(&accountsv1.LinkOrganizationRequest{
			Service: ownsand, OrganizationId: org, BillingAccountId: accountID, UserId: userID, OrganizationName: "Org",
		}))
		return err
	}
	wantCode(t, try(f.AccountID, f.Viewer.ID, newOrg(t)), connect.CodePermissionDenied)
	wantCode(t, try(f.AccountID, f.Outsider.ID, newOrg(t)), connect.CodeNotFound)
	wantCode(t, try(newOrg(t), f.Owner.ID, newOrg(t)), connect.CodeNotFound)
	wantCode(t, try("not-a-uuid", f.Owner.ID, newOrg(t)), connect.CodeInvalidArgument)
	wantCode(t, try(f.AccountID, "", newOrg(t)), connect.CodeInvalidArgument)
	_, err := env.Service().LinkOrganization(ctx(), connect.NewRequest(&accountsv1.LinkOrganizationRequest{
		Service: ownsand, OrganizationId: newOrg(t), BillingAccountId: f.AccountID, UserId: f.Owner.ID, OrganizationName: "bad\x07name",
	}))
	wantCode(t, err, connect.CodeInvalidArgument)
	org := newOrg(t)
	wantCode(t, try(f.AccountID, f.Admin.ID, org), 0)
	wantCode(t, try(f.AccountID, f.Owner.ID, org), 0)
	// Linked elsewhere: already_exists, also for the owner of the other
	// billing account.
	wantCode(t, try(other.AccountID, other.Owner.ID, org), connect.CodeAlreadyExists)
	// A billing account with a link cannot be deleted; after the unlink it
	// can, and a deleted billing account is not_found.
	_, err = f.Owner.Client.DeleteBillingAccount(ctx(), connect.NewRequest(&accountsv1.DeleteBillingAccountRequest{BillingAccountId: f.AccountID}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	unlink(t, env, org)
	unlink(t, env, org) // idempotent
	unlink(t, env, newOrg(t))
	wantCode(t, try(other.AccountID, other.Owner.ID, org), 0)
	unlink(t, env, org)
	_, err = f.Owner.Client.DeleteBillingAccount(ctx(), connect.NewRequest(&accountsv1.DeleteBillingAccountRequest{BillingAccountId: f.AccountID}))
	wantCode(t, err, 0)
	wantCode(t, try(f.AccountID, f.Owner.ID, newOrg(t)), connect.CodeNotFound)

	// Concurrent links of one organization to two billing accounts: exactly
	// one wins.
	race := newOrg(t)
	third := newFixture(t, env)
	errs := make(chan error, 2)
	go func() { errs <- try(other.AccountID, other.Owner.ID, race) }()
	go func() { errs <- try(third.AccountID, third.Owner.ID, race) }()
	e1, e2 := <-errs, <-errs
	if e1 != nil {
		e1, e2 = e2, e1
	}
	if e1 != nil || connect.CodeOf(e2) != connect.CodeAlreadyExists {
		t.Fatalf("concurrent links: %v, %v", e1, e2)
	}

	logs := env.Logs.String()
	for _, kind := range []string{"organization_linked", "organization_unlinked"} {
		if !strings.Contains(logs, `"kind":"`+kind+`"`) {
			t.Errorf("no audit event %s", kind)
		}
	}
}

func TestListManageableBillingAccounts(t *testing.T) {
	env := testenv.New(t)
	user := newPerson(env, "multi")
	var ids []string
	for i := range 3 {
		ids = append(ids, createAccount(t, user, fmt.Sprintf("Mine %d", i)).GetBillingAccountId())
	}
	f := newFixture(t, env)
	join(t, env, f.Owner, f.AccountID, user, roleAdmin)
	g := newFixture(t, env)
	join(t, env, g.Owner, g.AccountID, user, roleViewer)
	ids = append(ids, f.AccountID)

	var got []string
	token, firstToken := "", ""
	for {
		resp, err := env.Service().ListManageableBillingAccounts(ctx(), connect.NewRequest(&accountsv1.ListManageableBillingAccountsRequest{
			UserId: user.ID, PageSize: 3, PageToken: token,
		}))
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range resp.Msg.GetBillingAccounts() {
			want := roleOwner
			if a.GetBillingAccountId() == f.AccountID {
				want = roleAdmin
			}
			if a.GetCallerRole() != want {
				t.Fatalf("caller_role of %s = %s", a.GetBillingAccountId(), a.GetCallerRole())
			}
			got = append(got, a.GetBillingAccountId())
		}
		if firstToken == "" {
			firstToken = resp.Msg.GetNextPageToken()
		}
		if token = resp.Msg.GetNextPageToken(); token == "" {
			break
		}
	}
	if strings.Join(got, ",") != strings.Join(ids, ",") {
		t.Fatalf("accounts = %v, want %v (in ID order, without the viewer account)", got, ids)
	}

	// The last page has no continuation token, so the page token checks use
	// the token that the first page returned (3 of 4 accounts per page).
	if firstToken == "" {
		t.Fatal("first page returned no next_page_token")
	}
	list := func(userID, pageToken string) (*accountsv1.ListManageableBillingAccountsResponse, error) {
		resp, err := env.Service().ListManageableBillingAccounts(ctx(), connect.NewRequest(&accountsv1.ListManageableBillingAccountsRequest{
			UserId: userID, PageSize: 3, PageToken: pageToken,
		}))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}
	// The token continues the listing for the user it was issued to.
	next, err := list(user.ID, firstToken)
	if err != nil {
		t.Fatalf("first-page token for the original user: %v", err)
	}
	if len(next.GetBillingAccounts()) != 1 || next.GetBillingAccounts()[0].GetBillingAccountId() != ids[3] {
		t.Fatalf("continuation page = %v, want only %s", next.GetBillingAccounts(), ids[3])
	}
	// The same unmodified token is scoped to its user: another user gets
	// invalid_argument.
	_, err = list(newPerson(env, "other").ID, firstToken)
	wantCode(t, err, connect.CodeInvalidArgument)
	// A malformed token is rejected independently of the scope check.
	_, err = list(user.ID, "x")
	wantCode(t, err, connect.CodeInvalidArgument)
	_, err = list(user.ID, firstToken+"x")
	wantCode(t, err, connect.CodeInvalidArgument)
}

func TestGetOrganizationBillingAndUsageViews(t *testing.T) {
	env := testenv.New(t)
	f := newFixture(t, env)
	subscribe(t, env, f)
	org1, org2 := newOrg(t), newOrg(t)
	_, err := env.Service().GetOrganizationBilling(ctx(), connect.NewRequest(&accountsv1.GetOrganizationBillingRequest{Service: ownsand, OrganizationId: org1}))
	wantCode(t, err, connect.CodeNotFound)
	link(t, env, f.AccountID, org1, f.Owner.ID, "First")
	link(t, env, f.AccountID, org2, f.Owner.ID, "Second")
	env.Clock.Advance(15 * time.Minute)
	w := window(env.Clock.Now()).Add(-billing.WindowLength)
	reportUsage(t, env.Service(),
		report(org1, meterEgress, w, 100, 10_000),
		report(org1, meterEgress, w.Add(-billing.WindowLength), 50, 5_000),
		report(org1, meterClassB, w, 3, 30),
		report(org2, meterClassA, w, 9, 90),
	)

	resp, err := env.Service().GetOrganizationBilling(ctx(), connect.NewRequest(&accountsv1.GetOrganizationBillingRequest{Service: ownsand, OrganizationId: org1}))
	if err != nil {
		t.Fatal(err)
	}
	m := resp.Msg
	if m.GetBillingAccountId() != f.AccountID || m.GetBillingAccountName() != "Fixture" || m.GetBalanceUsdMicros() != 10*usd-15_030-90 ||
		m.GetStatus().GetStatus() != allowed || !m.GetMonthStart().AsTime().Equal(billing.MonthStart(time.Now())) {
		t.Fatalf("GetOrganizationBilling = %v", m)
	}
	var meters []string
	for _, mu := range m.GetMeterUsages() {
		meters = append(meters, fmt.Sprintf("%s=%d/%d", mu.GetMeter(), mu.GetQuantity(), mu.GetAmountUsdMicros()))
	}
	sameMonth := !w.Add(-billing.WindowLength).Before(billing.MonthStart(w))
	wantEgress := "METER_OWNSAND_RELAY_EGRESS=150/15000"
	if !sameMonth {
		wantEgress = "METER_OWNSAND_RELAY_EGRESS=100/10000"
	}
	if got := strings.Join(meters, " "); got != wantEgress+" METER_OWNSAND_R2_STORAGE=0/0 METER_OWNSAND_CLASS_A_RPC=0/0 METER_OWNSAND_CLASS_B_RPC=3/30" {
		t.Fatalf("meter usages = %s", got)
	}

	// Every member may list the linked organizations, in link order.
	for _, p := range []person{f.Owner, f.Admin, f.Viewer} {
		resp, err := env.BillingUsage(p.ID).ListLinkedOrganizations(ctx(), connect.NewRequest(&accountsv1.ListLinkedOrganizationsRequest{BillingAccountId: f.AccountID, PageSize: 1}))
		if err != nil {
			t.Fatalf("%s: %v", p.ID, err)
		}
		l := resp.Msg.GetLinkedOrganizations()
		if len(l) != 1 || l[0].GetOrganizationId() != org1 || l[0].GetOrganizationName() != "First" || l[0].GetStatus().GetStatus() != allowed ||
			resp.Msg.GetNextPageToken() == "" {
			t.Fatalf("first page = %v", resp.Msg)
		}
		next, err := env.BillingUsage(p.ID).ListLinkedOrganizations(ctx(), connect.NewRequest(&accountsv1.ListLinkedOrganizationsRequest{
			BillingAccountId: f.AccountID, PageSize: 1, PageToken: resp.Msg.GetNextPageToken()}))
		if err != nil || len(next.Msg.GetLinkedOrganizations()) != 1 || next.Msg.GetLinkedOrganizations()[0].GetOrganizationId() != org2 {
			t.Fatalf("second page = %v, %v", next, err)
		}
	}
	_, err = env.BillingUsage(f.Outsider.ID).ListLinkedOrganizations(ctx(), connect.NewRequest(&accountsv1.ListLinkedOrganizationsRequest{BillingAccountId: f.AccountID}))
	wantCode(t, err, connect.CodeNotFound)

	// The monthly summary by service, organization and meter, with the
	// total of every page.
	year, month := int32(w.Year()), int32(w.Month())
	summary := func(p person, token string, size int32) (*accountsv1.ListUsageSummaryResponse, error) {
		resp, err := env.BillingUsage(p.ID).ListUsageSummary(ctx(), connect.NewRequest(&accountsv1.ListUsageSummaryRequest{
			BillingAccountId: f.AccountID, Year: year, Month: month, PageSize: size, PageToken: token}))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}
	var rows []string
	var total int64
	token := ""
	for {
		page, err := summary(f.Viewer, token, 2)
		if err != nil {
			t.Fatal(err)
		}
		total = page.GetTotalAmountUsdMicros()
		for _, r := range page.GetRows() {
			rows = append(rows, fmt.Sprintf("%s/%s/%d", r.GetOrganizationId()[len(r.GetOrganizationId())-4:], r.GetMeter(), r.GetQuantity()))
		}
		if token = page.GetNextPageToken(); token == "" {
			break
		}
	}
	wantTotal := int64(15_000 + 30 + 90)
	if !sameMonth {
		wantTotal -= 5_000
	}
	if len(rows) != 3 || total != wantTotal {
		t.Fatalf("summary rows = %v with total %d, want 3 rows with %d", rows, total, wantTotal)
	}
	_, err = summary(f.Outsider, "", 0)
	wantCode(t, err, connect.CodeNotFound)
	for _, bad := range [][2]int32{{1999, 1}, {10000, 1}, {2026, 0}, {2026, 13}} {
		_, err := env.BillingUsage(f.Owner.ID).ListUsageSummary(ctx(), connect.NewRequest(&accountsv1.ListUsageSummaryRequest{
			BillingAccountId: f.AccountID, Year: bad[0], Month: bad[1]}))
		wantCode(t, err, connect.CodeInvalidArgument)
	}
	// A future month has no rows.
	future, err := env.BillingUsage(f.Owner.ID).ListUsageSummary(ctx(), connect.NewRequest(&accountsv1.ListUsageSummaryRequest{
		BillingAccountId: f.AccountID, Year: 9999, Month: 12}))
	if err != nil || len(future.Msg.GetRows()) != 0 || future.Msg.GetTotalAmountUsdMicros() != 0 {
		t.Fatalf("future month = %v, %v", future, err)
	}
}

// pinMidMonth moves the clock to noon UTC on the 15th of the current month,
// or of the next month when that moment has passed. A test that reads usage
// by the clock month and by the month of its windows calls it before the
// fixture exists. Otherwise the 15-minute advance of sameMonthWindows
// crosses a month end when the test starts shortly before the end of a UTC
// month, and GetOrganizationBilling then reads a month that has none of
// the windows.
func pinMidMonth(env *testenv.Env) {
	now := env.Clock.Now().UTC()
	target := time.Date(now.Year(), now.Month(), 15, 12, 0, 0, 0, time.UTC)
	if !target.After(now) {
		target = target.AddDate(0, 1, 0)
	}
	env.Clock.Advance(target.Sub(now))
}

// sameMonthWindows advances the clock and returns two usage windows that
// start in the same UTC calendar month, the later one first.
func sameMonthWindows(env *testenv.Env) (w, earlier time.Time) {
	env.Clock.Advance(15 * time.Minute)
	for {
		w = window(env.Clock.Now()).Add(-billing.WindowLength)
		earlier = w.Add(-billing.WindowLength)
		if !earlier.Before(billing.MonthStart(w)) {
			return w, earlier
		}
		env.Clock.Advance(billing.WindowLength)
	}
}

// monthlyUsage reads the usage views of a linked organization and billing
// account for the month of w: the meter usages of GetOrganizationBilling as
// "meter=quantity/amount", the rows of ListUsageSummary (read page by page
// with 2 rows per page) as "organization suffix/meter=quantity/amount" and
// the total amount of the summary.
func monthlyUsage(t *testing.T, env *testenv.Env, f fixture, org string, w time.Time) (meters, rows map[string]string, total int64) {
	t.Helper()
	resp, err := env.Service().GetOrganizationBilling(ctx(), connect.NewRequest(&accountsv1.GetOrganizationBillingRequest{Service: ownsand, OrganizationId: org}))
	if err != nil {
		t.Fatalf("GetOrganizationBilling: %v", err)
	}
	meters = map[string]string{}
	for _, mu := range resp.Msg.GetMeterUsages() {
		meters[mu.GetMeter().String()] = fmt.Sprintf("%d/%d", mu.GetQuantity(), mu.GetAmountUsdMicros())
	}
	rows = map[string]string{}
	token := ""
	for pages := 0; ; pages++ {
		page, err := env.BillingUsage(f.Viewer.ID).ListUsageSummary(ctx(), connect.NewRequest(&accountsv1.ListUsageSummaryRequest{
			BillingAccountId: f.AccountID, Year: int32(w.Year()), Month: int32(w.Month()), PageSize: 2, PageToken: token}))
		if err != nil {
			t.Fatalf("ListUsageSummary: %v", err)
		}
		if pages > 0 && page.Msg.GetTotalAmountUsdMicros() != total {
			t.Fatalf("total changed between pages: %d then %d", total, page.Msg.GetTotalAmountUsdMicros())
		}
		total = page.Msg.GetTotalAmountUsdMicros()
		for _, r := range page.Msg.GetRows() {
			id := r.GetOrganizationId()
			rows[fmt.Sprintf("%s/%s", id[len(id)-4:], r.GetMeter())] = fmt.Sprintf("%d/%d", r.GetQuantity(), r.GetAmountUsdMicros())
		}
		if token = page.Msg.GetNextPageToken(); token == "" {
			return meters, rows, total
		}
	}
}

func storedSum(t *testing.T, env *testenv.Env, column, org string, meter accountsv1.Meter) string {
	t.Helper()
	key, _, _ := billing.MeterKey(meter)
	var sum string
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT COALESCE(SUM(`+column+`), 0)::text FROM usage_reports WHERE organization_id = $1 AND meter = $2`,
		org, key).Scan(&sum); err != nil {
		t.Fatal(err)
	}
	return sum
}

// A monthly quantity sum above the int64 range saturates at the maximum in
// both read RPCs instead of failing them; a sum of exactly the maximum, the
// other meters, the paging and the total are unchanged, and ReportUsage and
// the stored rows stay exact.
func TestUsageQuantitySumSaturates(t *testing.T) {
	env := testenv.New(t)
	pinMidMonth(env)
	f := newFixture(t, env)
	subscribe(t, env, f)
	org1, org2 := newOrg(t), newOrg(t)
	link(t, env, f.AccountID, org1, f.Owner.ID, "First")
	link(t, env, f.AccountID, org2, f.Owner.ID, "Second")
	w, earlier := sameMonthWindows(env)
	// reportUsage fails the test when a report fails, also the one that
	// pushes a sum beyond the int64 range.
	reportUsage(t, env.Service(),
		report(org1, meterEgress, w, math.MaxInt64, 10),
		report(org1, meterEgress, earlier, 2, 20),
		report(org1, meterStorage, w, math.MaxInt64-5, 30),
		report(org1, meterStorage, earlier, 5, 40),
		report(org1, meterClassA, w, 9, 50),
		report(org1, meterClassA, earlier, 1, 60),
		report(org2, meterClassB, w, 7, 70),
	)

	meters, rows, total := monthlyUsage(t, env, f, org1, w)
	wantMeters := map[string]string{
		"METER_OWNSAND_RELAY_EGRESS": fmt.Sprintf("%d/30", int64(math.MaxInt64)), // saturated
		"METER_OWNSAND_R2_STORAGE":   fmt.Sprintf("%d/70", int64(math.MaxInt64)), // exactly the maximum
		"METER_OWNSAND_CLASS_A_RPC":  "10/110",
		"METER_OWNSAND_CLASS_B_RPC":  "0/0",
	}
	for meter, want := range wantMeters {
		if meters[meter] != want {
			t.Errorf("GetOrganizationBilling %s = %q, want %q", meter, meters[meter], want)
		}
	}
	suffix := func(org string) string { return org[len(org)-4:] }
	wantRows := map[string]string{
		suffix(org1) + "/METER_OWNSAND_RELAY_EGRESS": fmt.Sprintf("%d/30", int64(math.MaxInt64)),
		suffix(org1) + "/METER_OWNSAND_R2_STORAGE":   fmt.Sprintf("%d/70", int64(math.MaxInt64)),
		suffix(org1) + "/METER_OWNSAND_CLASS_A_RPC":  "10/110",
		suffix(org2) + "/METER_OWNSAND_CLASS_B_RPC":  "7/70",
	}
	if fmt.Sprint(rows) != fmt.Sprint(wantRows) {
		t.Errorf("ListUsageSummary rows = %v, want %v", rows, wantRows)
	}
	if total != 10+20+30+40+50+60+70 {
		t.Errorf("total_amount_usd_micros = %d, want 280", total)
	}

	// The stored rows keep the exact quantities.
	if got := storedSum(t, env, "quantity", org1, meterEgress); got != "9223372036854775809" {
		t.Errorf("stored egress quantity sum = %s, want 9223372036854775809", got)
	}
}

// A monthly amount sum above the int64 range saturates in the same way,
// including the total of ListUsageSummary. The rows are inserted directly,
// because the ledger rejects a deduction that overflows the balance.
func TestUsageAmountSumSaturates(t *testing.T) {
	env := testenv.New(t)
	pinMidMonth(env)
	f := newFixture(t, env)
	subscribe(t, env, f)
	org := newOrg(t)
	link(t, env, f.AccountID, org, f.Owner.ID, "First")
	w, earlier := sameMonthWindows(env)
	var linkID string
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT organization_link_id::text FROM organization_links WHERE organization_id = $1 AND unlink_time IS NULL`, org).Scan(&linkID); err != nil {
		t.Fatal(err)
	}
	insert := func(meter accountsv1.Meter, start time.Time, quantity, amount int64) {
		t.Helper()
		id, err := uuidv7.NewString()
		if err != nil {
			t.Fatal(err)
		}
		key, _, _ := billing.MeterKey(meter)
		if _, err := env.DB.Pool.Exec(ctx(), `INSERT INTO usage_reports
			(usage_report_id, service, organization_id, meter, window_start, quantity, amount_usd_micros, billing_account_id, organization_link_id, create_time)
			VALUES ($1, 'ownsand', $2, $3, $4, $5, $6, $7, $8, now())`, id, org, key, start, quantity, amount, f.AccountID, linkID); err != nil {
			t.Fatal(err)
		}
	}
	insert(meterEgress, w, 1, math.MaxInt64)
	insert(meterEgress, earlier, 2, math.MaxInt64)
	insert(meterStorage, w, 3, math.MaxInt64-5)
	insert(meterStorage, earlier, 4, 5)
	insert(meterClassA, w, 9, 10)
	insert(meterClassA, earlier, 1, 20)

	meters, rows, total := monthlyUsage(t, env, f, org, w)
	maxInt := int64(math.MaxInt64)
	wantMeters := map[string]string{
		"METER_OWNSAND_RELAY_EGRESS": fmt.Sprintf("3/%d", maxInt), // saturated
		"METER_OWNSAND_R2_STORAGE":   fmt.Sprintf("7/%d", maxInt), // exactly the maximum
		"METER_OWNSAND_CLASS_A_RPC":  "10/30",
		"METER_OWNSAND_CLASS_B_RPC":  "0/0",
	}
	for meter, want := range wantMeters {
		if meters[meter] != want {
			t.Errorf("GetOrganizationBilling %s = %q, want %q", meter, meters[meter], want)
		}
	}
	suffix := org[len(org)-4:]
	wantRows := map[string]string{
		suffix + "/METER_OWNSAND_RELAY_EGRESS": wantMeters["METER_OWNSAND_RELAY_EGRESS"],
		suffix + "/METER_OWNSAND_R2_STORAGE":   wantMeters["METER_OWNSAND_R2_STORAGE"],
		suffix + "/METER_OWNSAND_CLASS_A_RPC":  wantMeters["METER_OWNSAND_CLASS_A_RPC"],
	}
	if fmt.Sprint(rows) != fmt.Sprint(wantRows) {
		t.Errorf("ListUsageSummary rows = %v, want %v", rows, wantRows)
	}
	if total != maxInt {
		t.Errorf("total_amount_usd_micros = %d, want the saturated %d", total, maxInt)
	}
	if got := storedSum(t, env, "amount_usd_micros", org, meterEgress); got != "18446744073709551614" {
		t.Errorf("stored egress amount sum = %s, want 18446744073709551614", got)
	}
}
