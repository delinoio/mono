// SPDX-License-Identifier: FSL-1.1-ALv2

package accountstest

import (
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
)

func TestGetOrganizationBillingCountsOnlyTheCurrentMonth(t *testing.T) {
	const (
		org     = "org-1"
		user    = "user-1"
		account = "ba-1"
	)
	now := time.Date(2026, time.March, 31, 23, 30, 0, 0, time.UTC)
	f := New(t, func() time.Time { return now })
	f.AddAccount(account, "Account", map[string]accountsv1.BillingAccountRole{user: accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_OWNER})
	c := f.Client()
	ctx := t.Context()
	if _, err := c.LinkOrganization(ctx, connect.NewRequest(&accountsv1.LinkOrganizationRequest{BillingAccountId: account, OrganizationId: org, OrganizationName: "Org", UserId: user})); err != nil {
		t.Fatal(err)
	}

	meter := accountsv1.Meter_METER_OWNSAND_CLASS_A_RPC
	report := func(window time.Time, quantity, amount int64) *accountsv1.UsageReportResult {
		t.Helper()
		resp, err := c.ReportUsage(ctx, connect.NewRequest(&accountsv1.ReportUsageRequest{Reports: []*accountsv1.UsageReport{{
			Service: accountsv1.ServiceId_SERVICE_ID_OWNSAND, OrganizationId: org, Meter: meter, WindowStart: timestamppb.New(window), Quantity: quantity, AmountUsdMicros: amount}}}))
		if err != nil {
			t.Fatal(err)
		}
		return resp.Msg.GetResults()[0]
	}
	usage := func() (quantity, amount int64, monthStart time.Time) {
		t.Helper()
		resp, err := c.GetOrganizationBilling(ctx, connect.NewRequest(&accountsv1.GetOrganizationBillingRequest{OrganizationId: org}))
		if err != nil {
			t.Fatal(err)
		}
		for _, u := range resp.Msg.GetMeterUsages() {
			if u.GetMeter() == meter {
				return u.GetQuantity(), u.GetAmountUsdMicros(), resp.Msg.GetMonthStart().AsTime()
			}
		}
		t.Fatal("meter missing from the response")
		return 0, 0, time.Time{}
	}

	// The last window of March starts in March and counts for March.
	march := time.Date(2026, time.March, 31, 23, 0, 0, 0, time.UTC)
	report(march, 10, 100)
	if q, a, ms := usage(); q != 10 || a != 100 || !ms.Equal(time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("March usage = %d, %d, month start %s; want 10, 100", q, a, ms)
	}

	// After the rollover the March report no longer counts.
	now = time.Date(2026, time.April, 1, 0, 5, 0, 0, time.UTC)
	if q, a, ms := usage(); q != 0 || a != 0 || !ms.Equal(time.Date(2026, time.April, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("April usage before any April report = %d, %d, month start %s; want 0, 0", q, a, ms)
	}
	april := time.Date(2026, time.April, 1, 0, 0, 0, 0, time.UTC)
	report(april, 3, 30)
	if q, a, _ := usage(); q != 3 || a != 30 {
		t.Fatalf("April usage = %d, %d; want 3, 30", q, a)
	}

	// A duplicate report is neither counted nor deducted again.
	f.mu.Lock()
	before := f.accounts[account].balance
	f.mu.Unlock()
	if res := report(april, 3, 30); !res.GetDuplicate() {
		t.Fatalf("duplicate result = %v", res)
	}
	f.mu.Lock()
	after := f.accounts[account].balance
	f.mu.Unlock()
	if after != before {
		t.Fatalf("balance after duplicate = %d, want %d", after, before)
	}
	if q, a, _ := usage(); q != 3 || a != 30 {
		t.Fatalf("April usage after duplicate = %d, %d; want 3, 30", q, a)
	}
}
