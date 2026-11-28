// SPDX-License-Identifier: FSL-1.1-ALv2

package accountstest

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
)

func TestListManageableBillingAccountsRejectsNegativePageSize(t *testing.T) {
	t.Parallel()
	const userID = "fixture-user"
	const accountID = "019b76da-a800-7000-8000-000000000001"
	const owner = accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_OWNER
	f := New(t, nil)
	f.AddAccount(accountID, "Account", map[string]accountsv1.BillingAccountRole{userID: owner})
	client := f.Client()

	for _, tc := range []struct {
		size  int32
		token string
	}{
		{size: -1},
		{size: math.MinInt32},
		{size: -1, token: "invalid token"},
	} {
		resp, err := client.ListManageableBillingAccounts(t.Context(), connect.NewRequest(&accountsv1.ListManageableBillingAccountsRequest{
			UserId: userID, PageSize: tc.size, PageToken: tc.token,
		}))
		var connectErr *connect.Error
		if resp != nil || !errors.As(err, &connectErr) || connectErr.Code() != connect.CodeInvalidArgument ||
			connectErr.Message() != "page_size must not be negative" {
			t.Errorf("size %d with token %q returned %v, %v; want no page and InvalidArgument with page_size must not be negative", tc.size, tc.token, resp, err)
		}
	}

	resp, err := client.ListManageableBillingAccounts(t.Context(), connect.NewRequest(&accountsv1.ListManageableBillingAccountsRequest{
		UserId: userID, PageSize: 1,
	}))
	if err != nil {
		t.Fatal(err)
	}
	accounts := resp.Msg.GetBillingAccounts()
	if len(accounts) != 1 || accounts[0].GetBillingAccountId() != accountID ||
		accounts[0].GetCallerRole() != owner || resp.Msg.GetNextPageToken() != "" {
		t.Fatalf("size 1 returned %v; want the owner's account and an empty token", resp.Msg)
	}
}

func TestListManageableBillingAccountsPaginates(t *testing.T) {
	t.Parallel()
	const userID = "fixture-user"
	const otherUserID = "other-user"
	const (
		owner  = accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_OWNER
		admin  = accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_ADMIN
		viewer = accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_VIEWER
	)
	f := New(t, nil)
	type accountRow struct {
		id   string
		role accountsv1.BillingAccountRole
	}
	var want [201]accountRow
	for i := len(want); i >= 1; i-- {
		id := fmt.Sprintf("019b76da-a800-7000-8000-%012d", i)
		role := owner
		if i%2 == 0 {
			role = admin
		}
		want[i-1] = accountRow{id: id, role: role}
		f.AddAccount(id, "Manageable", map[string]accountsv1.BillingAccountRole{userID: role, otherUserID: role})
	}
	f.AddAccount("019b76da-a800-7000-8000-000000000202", "Viewable", map[string]accountsv1.BillingAccountRole{userID: viewer})
	client := f.Client()

	checkPage := func(size int32, token string, rows []accountRow, wantNext bool) string {
		t.Helper()
		resp, err := client.ListManageableBillingAccounts(t.Context(), connect.NewRequest(&accountsv1.ListManageableBillingAccountsRequest{
			UserId: userID, PageSize: size, PageToken: token,
		}))
		if err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		accounts := resp.Msg.GetBillingAccounts()
		if len(accounts) != len(rows) {
			t.Fatalf("size %d returned %d accounts; want %d", size, len(accounts), len(rows))
		}
		for i, row := range rows {
			if id, role := accounts[i].GetBillingAccountId(), accounts[i].GetCallerRole(); id != row.id || role != row.role {
				t.Errorf("size %d row %d = (%q, %v); want (%q, %v)", size, i, id, role, row.id, row.role)
			}
		}
		next := resp.Msg.GetNextPageToken()
		if (next != "") != wantNext {
			t.Fatalf("size %d returned token %q; want a continuation token: %t", size, next, wantNext)
		}
		return next
	}

	checkPage(0, "", want[:50], true)
	checkPage(1, "", want[:1], true)
	checkPage(137, "", want[:137], true)
	checkPage(200, "", want[:200], true)
	next := checkPage(201, "", want[:200], true)
	checkPage(math.MaxInt32, "", want[:200], true)
	checkPage(201, next, want[200:], false)

	resp, err := client.ListManageableBillingAccounts(t.Context(), connect.NewRequest(&accountsv1.ListManageableBillingAccountsRequest{
		UserId: otherUserID, PageSize: 201, PageToken: next,
	}))
	if resp != nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("another user's token returned %v, %v; want no page and InvalidArgument", resp, err)
	}
}

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
	balance := func() int64 {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.accounts[account].balance
	}

	// A March window before the link still counts for March.
	march := time.Date(2026, time.March, 31, 23, 0, 0, 0, time.UTC)
	if res := report(march, 10, 100); !res.GetUnattributed() || res.GetBillingAccountId() != "" {
		t.Fatalf("March result = %v, want unattributed with no account", res)
	}
	if got := balance(); got != 10_000_000 {
		t.Fatalf("balance after unattributed March report = %d, want 10000000", got)
	}
	if q, a, ms := usage(); q != 10 || a != 100 || !ms.Equal(time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("March usage = %d, %d, month start %s; want 10, 100", q, a, ms)
	}

	// After the rollover the March report no longer counts.
	now = time.Date(2026, time.April, 1, 0, 5, 0, 0, time.UTC)
	if q, a, ms := usage(); q != 0 || a != 0 || !ms.Equal(time.Date(2026, time.April, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("April usage before any April report = %d, %d, month start %s; want 0, 0", q, a, ms)
	}
	april := time.Date(2026, time.April, 1, 0, 0, 0, 0, time.UTC)
	if res := report(april, 3, 30); res.GetUnattributed() || res.GetBillingAccountId() != account {
		t.Fatalf("April result = %v, want attributed to %s", res, account)
	}
	if got := balance(); got != 9_999_970 {
		t.Fatalf("balance after April report = %d, want 9999970", got)
	}
	if q, a, _ := usage(); q != 3 || a != 30 {
		t.Fatalf("April usage = %d, %d; want 3, 30", q, a)
	}

	// A duplicate report is neither counted nor deducted again.
	before := balance()
	if res := report(april, 3, 30); !res.GetDuplicate() {
		t.Fatalf("duplicate result = %v", res)
	}
	if after := balance(); after != before {
		t.Fatalf("balance after duplicate = %d, want %d", after, before)
	}
	if q, a, _ := usage(); q != 3 || a != 30 {
		t.Fatalf("April usage after duplicate = %d, %d; want 3, 30", q, a)
	}
}

func TestGetOrganizationBillingSaturatesMonthlyUsage(t *testing.T) {
	t.Parallel()

	const (
		org            = "org-1"
		user           = "user-1"
		account        = "ba-1"
		initialBalance = 10_000_000
		service        = accountsv1.ServiceId_SERVICE_ID_OWNSAND
		meter          = accountsv1.Meter_METER_OWNSAND_CLASS_A_RPC
		otherMeter     = accountsv1.Meter_METER_OWNSAND_CLASS_B_RPC
		unusedMeter    = accountsv1.Meter_METER_OWNSAND_RELAY_EGRESS
	)
	type usage struct{ quantity, amount int64 }
	for _, tc := range []struct {
		name        string
		first       usage
		second      usage
		want        usage
		wantBalance int64
	}{
		{"quantity overflow", usage{math.MaxInt64, 10}, usage{1, 20}, usage{math.MaxInt64, 30}, initialBalance - 30},
		{"amount overflow", usage{2, math.MaxInt64}, usage{3, 1}, usage{5, math.MaxInt64}, initialBalance - math.MaxInt64 - 1},
		{"exact limit", usage{math.MaxInt64 - 1, math.MaxInt64 - 1}, usage{1, 1}, usage{math.MaxInt64, math.MaxInt64}, initialBalance - math.MaxInt64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Date(2026, time.April, 1, 12, 0, 0, 0, time.UTC)
			var minute atomic.Int64
			f := New(t, func() time.Time { return start.Add(time.Duration(minute.Load()) * time.Minute) })
			f.AddAccount(account, "Account", map[string]accountsv1.BillingAccountRole{user: accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_OWNER})
			c := f.Client()
			ctx := t.Context()
			if _, err := c.LinkOrganization(ctx, connect.NewRequest(&accountsv1.LinkOrganizationRequest{
				BillingAccountId: account, OrganizationId: org, OrganizationName: "Org", UserId: user,
			})); err != nil {
				t.Fatal(err)
			}
			minute.Store(15)
			firstWindow := start.Add(5 * time.Minute)
			secondWindow := start.Add(10 * time.Minute)
			report := func(m accountsv1.Meter, window time.Time, value usage, duplicate bool) {
				t.Helper()
				ts := timestamppb.New(window)
				resp, err := c.ReportUsage(ctx, connect.NewRequest(&accountsv1.ReportUsageRequest{Reports: []*accountsv1.UsageReport{{
					Service: service, OrganizationId: org, Meter: m, WindowStart: ts, Quantity: value.quantity, AmountUsdMicros: value.amount,
				}}}))
				if err != nil {
					t.Fatal(err)
				}
				want := &accountsv1.UsageReportResult{
					Service: service, OrganizationId: org, Meter: m, WindowStart: ts, BillingAccountId: account, Duplicate: duplicate,
				}
				if results := resp.Msg.GetResults(); len(results) != 1 || !proto.Equal(results[0], want) {
					t.Fatalf("ReportUsage results = %v, want [%v]", results, want)
				}
			}
			checkBilling := func(wantBalance int64, wantOther usage) {
				t.Helper()
				resp, err := c.GetOrganizationBilling(ctx, connect.NewRequest(&accountsv1.GetOrganizationBillingRequest{OrganizationId: org}))
				if err != nil {
					t.Fatal(err)
				}
				if got := resp.Msg.GetBalanceUsdMicros(); got != wantBalance {
					t.Errorf("balance = %d, want %d", got, wantBalance)
				}
				gotUsages := make(map[accountsv1.Meter]usage)
				for _, u := range resp.Msg.GetMeterUsages() {
					gotUsages[u.GetMeter()] = usage{u.GetQuantity(), u.GetAmountUsdMicros()}
				}
				for m, want := range map[accountsv1.Meter]usage{meter: tc.want, otherMeter: wantOther, unusedMeter: {}} {
					if got, ok := gotUsages[m]; !ok || got != want {
						t.Errorf("usage for %s = %+v (present: %t), want %+v", m, got, ok, want)
					}
				}
			}

			report(meter, firstWindow, tc.first, false)
			report(meter, secondWindow, tc.second, false)
			checkBilling(tc.wantBalance, usage{})

			report(meter, secondWindow, tc.second, true)
			checkBilling(tc.wantBalance, usage{})

			other := usage{7, 9}
			report(otherMeter, firstWindow, other, false)
			checkBilling(tc.wantBalance-other.amount, other)
		})
	}
}

func TestAddMonthlyUsageWithNegativeTotal(t *testing.T) {
	t.Parallel()

	if got := addMonthlyUsage(math.MinInt64, math.MaxInt64); got != -1 {
		t.Errorf("addMonthlyUsage(math.MinInt64, math.MaxInt64) = %d, want -1", got)
	}
}

func TestReportUsageRejectsMalformedReports(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.March, 31, 23, 30, 0, 0, time.UTC)
	f := New(t, func() time.Time { return now })
	client := f.Client()
	valid := &accountsv1.UsageReport{
		Service: accountsv1.ServiceId_SERVICE_ID_OWNSAND, OrganizationId: "org-1",
		Meter: accountsv1.Meter_METER_OWNSAND_CLASS_A_RPC, WindowStart: timestamppb.New(now.Add(-time.Hour)),
	}
	for name, change := range map[string]func(*accountsv1.UsageReport){
		"missing service": func(r *accountsv1.UsageReport) { r.Service = accountsv1.ServiceId_SERVICE_ID_UNSPECIFIED },
		"unknown service": func(r *accountsv1.UsageReport) { r.Service = 99 },
		"missing meter":   func(r *accountsv1.UsageReport) { r.Meter = accountsv1.Meter_METER_UNSPECIFIED },
		"unknown meter":   func(r *accountsv1.UsageReport) { r.Meter = 99 },
		"empty org":       func(r *accountsv1.UsageReport) { r.OrganizationId = "" },
		"long org":        func(r *accountsv1.UsageReport) { r.OrganizationId = strings.Repeat("a", 129) },
		"non-ASCII org":   func(r *accountsv1.UsageReport) { r.OrganizationId = "org-é" },
		"ASCII DEL":       func(r *accountsv1.UsageReport) { r.OrganizationId = "org\x7f" },
		"ASCII control":   func(r *accountsv1.UsageReport) { r.OrganizationId = "org\n" },
		"missing window":  func(r *accountsv1.UsageReport) { r.WindowStart = nil },
		"invalid timestamp": func(r *accountsv1.UsageReport) {
			r.WindowStart = timestamppb.New(time.Date(0, time.December, 31, 0, 0, 0, 0, time.UTC))
		},
		"unaligned seconds": func(r *accountsv1.UsageReport) { r.WindowStart.Seconds++ },
		"unaligned nanos":   func(r *accountsv1.UsageReport) { r.WindowStart.Nanos++ },
		"future window":     func(r *accountsv1.UsageReport) { r.WindowStart = timestamppb.New(now) },
	} {
		t.Run(name, func(t *testing.T) {
			bad := proto.CloneOf(valid)
			change(bad)
			_, err := client.ReportUsage(t.Context(), connect.NewRequest(&accountsv1.ReportUsageRequest{
				Reports: []*accountsv1.UsageReport{valid, bad, {}},
			}))
			if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "reports[1]") {
				t.Fatalf("ReportUsage error = %v, want InvalidArgument naming reports[1]", err)
			}
		})
	}
}

func TestReportUsageBatchAndWindowLimits(t *testing.T) {
	t.Parallel()
	// At this clock reading, the first window ends exactly one minute ahead.
	now := time.Date(2026, time.March, 31, 23, 29, 0, 0, time.UTC)
	f := New(t, func() time.Time { return now })
	client := f.Client()
	reports := make([]*accountsv1.UsageReport, 501)
	for i := range reports {
		reports[i] = &accountsv1.UsageReport{
			Service: accountsv1.ServiceId_SERVICE_ID_OWNSAND, OrganizationId: " ",
			Meter:       accountsv1.Meter_METER_OWNSAND_CLASS_A_RPC,
			WindowStart: timestamppb.New(now.Add(-4*time.Minute - time.Duration(i)*5*time.Minute)),
		}
	}
	reports[1].OrganizationId = strings.Repeat("~", 128)
	reports[499].WindowStart = timestamppb.New(time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC))
	send := func(batch []*accountsv1.UsageReport) (*connect.Response[accountsv1.ReportUsageResponse], error) {
		return client.ReportUsage(t.Context(), connect.NewRequest(&accountsv1.ReportUsageRequest{Reports: batch}))
	}
	for _, size := range []int{0, 501} {
		_, err := send(reports[:size])
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("ReportUsage with %d reports: error = %v, want InvalidArgument", size, err)
		}
	}
	f.mu.Lock()
	usage, reported := len(f.usage), len(f.reported)
	f.mu.Unlock()
	if usage != 0 || reported != 0 {
		t.Fatalf("after rejected batch sizes: usage buckets = %d, reported keys = %d, want both zero", usage, reported)
	}
	resp, err := send(reports[:500])
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Msg.GetResults(); len(got) != 500 || got[0].GetDuplicate() || got[499].GetDuplicate() {
		t.Fatalf("ReportUsage with 500 reports returned %d results, want 500 new results", len(got))
	}
}

func TestReportUsageRejectsBatchBeforeWriting(t *testing.T) {
	t.Parallel()
	const (
		org     = "org-1"
		user    = "user-1"
		account = "ba-1"
		balance = 10_000_000
		service = accountsv1.ServiceId_SERVICE_ID_OWNSAND
		meter   = accountsv1.Meter_METER_OWNSAND_CLASS_A_RPC
	)
	start := time.Date(2026, time.March, 31, 22, 55, 0, 0, time.UTC)
	for _, tc := range []struct {
		name             string
		quantity, amount int64
	}{
		{name: "negative quantity", quantity: -1, amount: 3},
		{name: "negative amount", quantity: 1, amount: -3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var elapsed atomic.Int64
			f := New(t, func() time.Time { return start.Add(time.Duration(elapsed.Load())) })
			f.AddAccount(account, "Account", map[string]accountsv1.BillingAccountRole{user: accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_OWNER})
			client := f.Client()
			ctx := t.Context()
			link, err := client.LinkOrganization(ctx, connect.NewRequest(&accountsv1.LinkOrganizationRequest{
				Service: service, OrganizationId: org, BillingAccountId: account, UserId: user,
			}))
			if err != nil {
				t.Fatal(err)
			}
			elapsed.Store(int64(35 * time.Minute))
			reports := []*accountsv1.UsageReport{
				{Service: service, OrganizationId: org, Meter: meter, WindowStart: timestamppb.New(start.Add(5 * time.Minute)), Quantity: 10, AmountUsdMicros: 5},
				{Service: service, OrganizationId: org, Meter: meter, WindowStart: timestamppb.New(start.Add(10 * time.Minute)), Quantity: tc.quantity, AmountUsdMicros: tc.amount},
			}
			send := func() (*connect.Response[accountsv1.ReportUsageResponse], error) {
				return client.ReportUsage(ctx, connect.NewRequest(&accountsv1.ReportUsageRequest{Reports: reports}))
			}
			assertBilling := func(quantity, amount int64) {
				t.Helper()
				resp, err := client.GetOrganizationBilling(ctx, connect.NewRequest(&accountsv1.GetOrganizationBillingRequest{
					Service: service, OrganizationId: org,
				}))
				if err != nil {
					t.Fatal(err)
				}
				if got, want := resp.Msg.GetBalanceUsdMicros(), balance-amount; got != want {
					t.Fatalf("balance = %d, want %d", got, want)
				}
				want := &accountsv1.MeterUsage{Meter: meter, Quantity: quantity, AmountUsdMicros: amount}
				for _, got := range resp.Msg.GetMeterUsages() {
					if got.GetMeter() == meter {
						if !proto.Equal(got, want) {
							t.Fatalf("monthly usage = %v, want %v", got, want)
						}
						return
					}
				}
				t.Fatalf("monthly usage has no entry for %s", meter)
			}

			if _, err := send(); connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "reports[1]") {
				t.Fatalf("ReportUsage error = %v, want InvalidArgument naming reports[1]", err)
			}
			assertBilling(0, 0)

			reports[1].Quantity, reports[1].AmountUsdMicros = 1, 3
			for _, duplicate := range []bool{false, true} {
				resp, err := send()
				if err != nil {
					t.Fatal(err)
				}
				want := &accountsv1.ReportUsageResponse{Statuses: []*accountsv1.OrganizationBillingStatus{link.Msg.GetStatus()}}
				for _, r := range reports {
					want.Results = append(want.Results, &accountsv1.UsageReportResult{
						Service: service, OrganizationId: org, Meter: meter, WindowStart: r.GetWindowStart(),
						Duplicate: duplicate, BillingAccountId: account,
					})
				}
				if !proto.Equal(resp.Msg, want) {
					t.Fatalf("ReportUsage response = %v, want %v", resp.Msg, want)
				}
				assertBilling(11, 8)
			}
		})
	}
}

func TestReportUsageFailureInjectionPrecedesValidation(t *testing.T) {
	t.Parallel()
	var reads atomic.Int64
	f := New(t, func() time.Time {
		reads.Add(1)
		return time.Time{}
	})
	f.Fail(MethodReportUsage, connect.NewError(connect.CodeUnavailable, errors.New("injected failure")))
	_, err := f.Client().ReportUsage(t.Context(), connect.NewRequest(&accountsv1.ReportUsageRequest{}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("ReportUsage error = %v, want Unavailable", err)
	}
	if calls, clockReads := f.Calls(MethodReportUsage), reads.Load(); calls != 1 || clockReads != 0 {
		t.Fatalf("ReportUsage calls = %d and clock reads = %d, want 1 and 0", calls, clockReads)
	}
}

func TestListBillingStatusChangesPagination(t *testing.T) {
	t.Parallel()
	f := New(t, nil)
	const (
		account = "ba-1"
		user    = "user-1"
		service = accountsv1.ServiceId_SERVICE_ID_OWNSAND
	)
	f.AddAccount(account, "Account", map[string]accountsv1.BillingAccountRole{user: accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_OWNER})
	client := f.Client()
	link := func(sequence int) {
		t.Helper()
		org := fmt.Sprintf("org-%d", sequence)
		if _, err := client.LinkOrganization(t.Context(), connect.NewRequest(&accountsv1.LinkOrganizationRequest{
			Service: service, BillingAccountId: account, OrganizationId: org, UserId: user,
		})); err != nil {
			t.Fatalf("link %s: %v", org, err)
		}
	}
	link(1)
	for _, size := range []int32{-1, math.MinInt32} {
		resp, err := client.ListBillingStatusChanges(t.Context(), connect.NewRequest(&accountsv1.ListBillingStatusChangesRequest{
			Service: service, PageSize: size,
		}))
		var rpcErr *connect.Error
		if resp != nil || !errors.As(err, &rpcErr) || rpcErr.Code() != connect.CodeInvalidArgument || rpcErr.Message() != "page_size must not be negative" {
			t.Errorf("poll with page size %d returned response %v and error %v, want no response and invalid_argument: page_size must not be negative", size, resp, err)
		}
	}
	for sequence := 2; sequence <= 201; sequence++ {
		link(sequence)
	}

	for _, tc := range []struct {
		name     string
		after    int64
		size     int32
		count    int
		wantNext int64
	}{
		{name: "default", count: 50, wantNext: 50},
		{name: "one", size: 1, count: 1, wantNext: 1},
		{name: "maximum", size: 200, count: 200, wantNext: 200},
		{name: "capped", size: 201, count: 200, wantNext: 200},
		{name: "last page", after: 200, size: 200, count: 1, wantNext: 201},
		{name: "empty page", after: 201, size: 200, wantNext: 201},
		{name: "cursor beyond last status", after: 202, size: 200, wantNext: 202},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := client.ListBillingStatusChanges(t.Context(), connect.NewRequest(&accountsv1.ListBillingStatusChangesRequest{
				Service: service, AfterSequence: tc.after, PageSize: tc.size,
			}))
			if err != nil {
				t.Fatal(err)
			}
			if got := resp.Msg.GetNextSequence(); got != tc.wantNext {
				t.Errorf("next sequence = %d, want %d", got, tc.wantNext)
			}
			statuses := resp.Msg.GetStatuses()
			if len(statuses) != tc.count {
				t.Fatalf("status count = %d, want %d", len(statuses), tc.count)
			}
			for i, status := range statuses {
				sequence := tc.after + int64(i) + 1
				org := fmt.Sprintf("org-%d", sequence)
				if status.GetSequence() != sequence || status.GetOrganizationId() != org {
					t.Fatalf("status %d = %v, want sequence %d for %s", i, status, sequence, org)
				}
			}
		})
	}
}

func TestReportUsageRefreshesBillingStatusOnBalanceExhaustion(t *testing.T) {
	const (
		account   = "ba-1"
		user      = "owner"
		allowed   = accountsv1.BillingStatus_BILLING_STATUS_ALLOWED
		blocked   = accountsv1.BillingStatus_BILLING_STATUS_BLOCKED
		suspended = accountsv1.BillingStatus_BILLING_STATUS_SUSPENDED
		service   = accountsv1.ServiceId_SERVICE_ID_OWNSAND
	)
	orgs := []string{"org-1", "org-2"}
	start := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	type charge struct {
		windowOffset    time.Duration
		amount, balance int64
		duplicate       bool
	}
	tests := []struct {
		name         string
		initialPhase accountsv1.BillingStatus
		wantBlock    bool
		charges      []charge
	}{
		{
			name: "exact exhaustion then replay and further debit", initialPhase: allowed, wantBlock: true,
			charges: []charge{
				{amount: 10_000_000, balance: 0},
				{amount: 10_000_000, balance: 0, duplicate: true},
				{windowOffset: 5 * time.Minute, amount: 1, balance: -1},
			},
		},
		{
			name: "cross below zero", initialPhase: allowed, wantBlock: true,
			charges: []charge{{amount: 10_000_001, balance: -1}},
		},
		{
			name: "balance stays positive", initialPhase: allowed,
			charges: []charge{{amount: 9_999_999, balance: 1}},
		},
		{
			name: "explicitly blocked", initialPhase: blocked,
			charges: []charge{{amount: 10_000_000, balance: 0}},
		},
		{
			name: "explicitly suspended", initialPhase: suspended,
			charges: []charge{{amount: 10_000_000, balance: 0}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var elapsed, reads atomic.Int64
			f := New(t, func() time.Time {
				reads.Add(1)
				return start.Add(time.Duration(elapsed.Load()))
			})
			f.AddAccount(account, "Account", map[string]accountsv1.BillingAccountRole{user: accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_OWNER})
			client := f.Client()
			ctx := t.Context()
			statuses := make([]*accountsv1.OrganizationBillingStatus, len(orgs))
			for i, org := range orgs {
				link, err := client.LinkOrganization(ctx, connect.NewRequest(&accountsv1.LinkOrganizationRequest{
					Service: service, OrganizationId: org, OrganizationName: org, BillingAccountId: account, UserId: user,
				}))
				if err != nil {
					t.Fatal(err)
				}
				statuses[i] = link.Msg.GetStatus()
			}
			if tt.initialPhase != allowed {
				f.SetAccountStatus(account, tt.initialPhase, start.Add(-72*time.Hour))
				for i, org := range orgs {
					statuses[i] = f.Status(org)
				}
			}
			cursor := statuses[len(statuses)-1].GetSequence()

			for i, c := range tt.charges {
				chargeTime := c.windowOffset + 5*time.Minute
				elapsed.Store(int64(chargeTime))
				before := reads.Load()
				report, err := client.ReportUsage(ctx, connect.NewRequest(&accountsv1.ReportUsageRequest{Reports: []*accountsv1.UsageReport{{
					Service: service, OrganizationId: orgs[0], Meter: accountsv1.Meter_METER_OWNSAND_CLASS_A_RPC,
					WindowStart: timestamppb.New(start.Add(c.windowOffset)), Quantity: 1, AmountUsdMicros: c.amount,
				}}}))
				if err != nil {
					t.Fatal(err)
				}
				if got := reads.Load() - before; got != 1 {
					t.Fatalf("ReportUsage read the clock %d times, want 1", got)
				}
				results := report.Msg.GetResults()
				if len(results) != 1 || results[0].GetDuplicate() != c.duplicate ||
					(!c.duplicate && results[0].GetBillingAccountId() != account) {
					t.Fatalf("charge %d: unexpected report results %v", i, results)
				}
				poll, err := client.ListBillingStatusChanges(ctx, connect.NewRequest(&accountsv1.ListBillingStatusChangesRequest{
					Service: service, AfterSequence: cursor,
				}))
				if err != nil {
					t.Fatal(err)
				}
				changes := poll.Msg.GetStatuses()
				wantCursor := cursor
				if tt.wantBlock && i == 0 {
					if len(changes) != len(orgs) {
						t.Fatalf("poll after %d = %v, want new statuses for both organizations", cursor, changes)
					}
					for j, change := range changes {
						blockTime := start.Add(chargeTime)
						if change.GetOrganizationId() != orgs[j] || change.GetBillingAccountId() != account ||
							change.GetStatus() != blocked || change.GetReason() != accountsv1.BillingStatusReason_BILLING_STATUS_REASON_NON_POSITIVE_BALANCE ||
							change.GetSequence() <= wantCursor || !change.GetBlockedSince().AsTime().Equal(blockTime) ||
							!change.GetSuspendTime().AsTime().Equal(blockTime.Add(72*time.Hour)) {
							t.Fatalf("unexpected status change: %v", change)
						}
						wantCursor = change.GetSequence()
					}
					statuses = changes
				} else if len(changes) != 0 {
					t.Fatalf("charge %d: poll after %d = %v, want no changes", i, cursor, changes)
				}
				if cursor = poll.Msg.GetNextSequence(); cursor != wantCursor {
					t.Fatalf("charge %d: next cursor = %d, want %d", i, cursor, wantCursor)
				}
				if got := report.Msg.GetStatuses(); len(got) != 1 || !proto.Equal(got[0], statuses[0]) {
					t.Fatalf("charge %d: report statuses = %v, want only %v", i, got, statuses[0])
				}
				for j, org := range orgs {
					billing, err := client.GetOrganizationBilling(ctx, connect.NewRequest(&accountsv1.GetOrganizationBillingRequest{
						Service: service, OrganizationId: org,
					}))
					if err != nil {
						t.Fatal(err)
					}
					if got := billing.Msg; got.GetBalanceUsdMicros() != c.balance || !proto.Equal(got.GetStatus(), statuses[j]) {
						t.Fatalf("charge %d: billing for %s = %v, want balance %d with status %v", i, org, got, c.balance, statuses[j])
					}
				}
			}
		})
	}
}

func TestReportUsageUsesBillingLinkHistory(t *testing.T) {
	const (
		org  = "org-1"
		user = "user-1"
		a    = "ba-a"
		b    = "ba-b"
	)
	type linkChange struct {
		minute        int64
		account, name string // An empty account unlinks the organization.
	}
	tests := []struct {
		name                        string
		changes                     []linkChange
		charged, active, activeName string
	}{
		{
			name:    "delayed after relink",
			changes: []linkChange{{0, a, "Org"}, {minute: 6}, {7, b, "Org"}},
			charged: a, active: b, activeName: "Org",
		},
		{
			name:    "delayed after unlink",
			changes: []linkChange{{0, a, "Org"}, {minute: 6}},
			charged: a,
		},
		{
			name:    "latest overlap ends inside window",
			changes: []linkChange{{0, a, "Org"}, {minute: 2}, {3, b, "Org"}, {minute: 4}},
			charged: b,
		},
		{
			name:    "unlink at window start",
			changes: []linkChange{{-2, a, "Org"}, {minute: 0}, {minute: 8}},
		},
		{
			name:    "link at window end",
			changes: []linkChange{{5, a, "Org"}},
			active:  a, activeName: "Org",
		},
		{
			name:    "ignore empty latest interval",
			changes: []linkChange{{0, a, "Org"}, {minute: 2}, {3, b, "Org"}, {minute: 3}},
			charged: a,
		},
		{
			name:    "link retry preserves start and updates name",
			changes: []linkChange{{0, a, "Org"}, {6, a, "Renamed"}},
			charged: a, active: a, activeName: "Renamed",
		},
	}
	window := time.Date(2026, time.March, 31, 12, 0, 0, 0, time.UTC)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var minute atomic.Int64
			f := New(t, func() time.Time { return window.Add(time.Duration(minute.Load()) * time.Minute) })
			members := map[string]accountsv1.BillingAccountRole{user: accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_OWNER}
			f.AddAccount(a, "Account A", members)
			f.AddAccount(b, "Account B", members)
			client := f.Client()
			for _, change := range tt.changes {
				minute.Store(change.minute)
				if change.account == "" {
					if _, err := client.UnlinkOrganization(t.Context(), connect.NewRequest(&accountsv1.UnlinkOrganizationRequest{OrganizationId: org})); err != nil {
						t.Fatal(err)
					}
				} else if _, err := client.LinkOrganization(t.Context(), connect.NewRequest(&accountsv1.LinkOrganizationRequest{
					OrganizationId: org, OrganizationName: change.name, BillingAccountId: change.account, UserId: user,
				})); err != nil {
					t.Fatal(err)
				}
			}
			if id, name, ok := f.Link(org); id != tt.active || name != tt.activeName || ok != (tt.active != "") {
				t.Fatalf("current link = (%q, %q, %t), want account %q and name %q", id, name, ok, tt.active, tt.activeName)
			}

			minute.Store(10)
			report := &accountsv1.UsageReport{
				Service: accountsv1.ServiceId_SERVICE_ID_OWNSAND, OrganizationId: org,
				Meter: accountsv1.Meter_METER_OWNSAND_CLASS_A_RPC, WindowStart: timestamppb.New(window),
				Quantity: 10, AmountUsdMicros: 100,
			}
			for _, duplicate := range []bool{false, true} {
				resp, err := client.ReportUsage(t.Context(), connect.NewRequest(&accountsv1.ReportUsageRequest{Reports: []*accountsv1.UsageReport{report}}))
				if err != nil {
					t.Fatal(err)
				}
				results := resp.Msg.GetResults()
				if len(results) != 1 || results[0].GetDuplicate() != duplicate {
					t.Fatalf("results = %v, want one result with duplicate=%t", results, duplicate)
				}
				if res := results[0]; !duplicate && (res.GetBillingAccountId() != tt.charged || res.GetUnattributed() != (tt.charged == "")) {
					t.Fatalf("result = %v, want billing account %q and unattributed=%t", res, tt.charged, tt.charged == "")
				}
				statuses := resp.Msg.GetStatuses()
				if tt.active == "" && len(statuses) != 0 || tt.active != "" && (len(statuses) != 1 || statuses[0].GetBillingAccountId() != tt.active) {
					t.Fatalf("current statuses = %v, want current account %q", statuses, tt.active)
				}
				for _, id := range []string{a, b} {
					want := int64(10_000_000)
					if id == tt.charged {
						want -= report.GetAmountUsdMicros()
					}
					f.mu.Lock()
					got := f.accounts[id].balance
					f.mu.Unlock()
					if got != want {
						t.Fatalf("account %s balance with duplicate=%t = %d, want %d", id, duplicate, got, want)
					}
				}
			}
		})
	}
}

func TestReportUsageReplaysOriginalAttribution(t *testing.T) {
	const (
		org            = "org-1"
		user           = "user-1"
		accountA       = "ba-a"
		accountB       = "ba-b"
		service        = accountsv1.ServiceId_SERVICE_ID_OWNSAND
		meter          = accountsv1.Meter_METER_OWNSAND_CLASS_A_RPC
		initialBalance = 10_000_000
	)
	window := time.Date(2026, time.April, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, originalAccount string
	}{
		{name: "charged", originalAccount: accountA},
		{name: "unattributed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var minute atomic.Int64
			f := New(t, func() time.Time { return window.Add(time.Duration(minute.Load()) * time.Minute) })
			members := map[string]accountsv1.BillingAccountRole{user: accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_OWNER}
			f.AddAccount(accountA, "Account A", members)
			f.AddAccount(accountB, "Account B", members)
			client := f.Client()
			link := func(accountID string) *accountsv1.OrganizationBillingStatus {
				t.Helper()
				resp, err := client.LinkOrganization(t.Context(), connect.NewRequest(&accountsv1.LinkOrganizationRequest{
					OrganizationId: org, OrganizationName: "Org", BillingAccountId: accountID, UserId: user,
				}))
				if err != nil {
					t.Fatal(err)
				}
				return resp.Msg.GetStatus()
			}
			report := func(reports ...*accountsv1.UsageReport) *accountsv1.ReportUsageResponse {
				t.Helper()
				resp, err := client.ReportUsage(t.Context(), connect.NewRequest(&accountsv1.ReportUsageRequest{Reports: reports}))
				if err != nil {
					t.Fatal(err)
				}
				return resp.Msg
			}
			assertResponse := func(got *accountsv1.ReportUsageResponse, status *accountsv1.OrganizationBillingStatus, results ...*accountsv1.UsageReportResult) {
				t.Helper()
				want := &accountsv1.ReportUsageResponse{Results: results}
				if status != nil {
					want.Statuses = []*accountsv1.OrganizationBillingStatus{status}
				}
				if !proto.Equal(got, want) {
					t.Fatalf("ReportUsage = %v, want %v", got, want)
				}
			}
			assertBilling := func(wantA, wantB, quantity, amount int64) {
				t.Helper()
				f.mu.Lock()
				gotA := f.accounts[accountA].balance
				f.mu.Unlock()
				if gotA != wantA {
					t.Fatalf("account A balance = %d, want %d", gotA, wantA)
				}
				resp, err := client.GetOrganizationBilling(t.Context(), connect.NewRequest(&accountsv1.GetOrganizationBillingRequest{OrganizationId: org}))
				if err != nil {
					t.Fatal(err)
				}
				if resp.Msg.GetBillingAccountId() != accountB || resp.Msg.GetBalanceUsdMicros() != wantB {
					t.Fatalf("current account and balance = %s, %d, want %s, %d", resp.Msg.GetBillingAccountId(), resp.Msg.GetBalanceUsdMicros(), accountB, wantB)
				}
				want := &accountsv1.MeterUsage{Meter: meter, Quantity: quantity, AmountUsdMicros: amount}
				for _, usage := range resp.Msg.GetMeterUsages() {
					if usage.GetMeter() == meter {
						if !proto.Equal(usage, want) {
							t.Fatalf("monthly usage = %v, want %v", usage, want)
						}
						return
					}
				}
				t.Fatalf("monthly usage has no entry for %s", meter)
			}

			var firstStatus *accountsv1.OrganizationBillingStatus
			wantA := int64(initialBalance)
			if tc.originalAccount != "" {
				firstStatus = link(tc.originalAccount)
				wantA -= 30
			}
			minute.Store(10)
			first := &accountsv1.UsageReport{
				Service: service, OrganizationId: org, Meter: meter, WindowStart: timestamppb.New(window),
				Quantity: 3, AmountUsdMicros: 30,
			}
			firstResult := &accountsv1.UsageReportResult{
				Service: service, OrganizationId: org, Meter: meter, WindowStart: first.GetWindowStart(),
				BillingAccountId: tc.originalAccount, Unattributed: tc.originalAccount == "",
			}
			assertResponse(report(first), firstStatus, firstResult)
			firstReplay := proto.CloneOf(firstResult)
			firstReplay.Duplicate = true
			assertResponse(report(first), firstStatus, firstReplay)

			if firstStatus != nil {
				if _, err := client.UnlinkOrganization(t.Context(), connect.NewRequest(&accountsv1.UnlinkOrganizationRequest{OrganizationId: org})); err != nil {
					t.Fatal(err)
				}
			}
			currentStatus := link(accountB)
			assertResponse(report(first), currentStatus, firstReplay)
			assertBilling(wantA, initialBalance, 3, 30)

			minute.Store(20)
			next := proto.CloneOf(first)
			next.WindowStart = timestamppb.New(window.Add(10 * time.Minute))
			changed := proto.CloneOf(next)
			changed.Quantity = 9
			changed.AmountUsdMicros = 90
			nextResult := &accountsv1.UsageReportResult{
				Service: service, OrganizationId: org, Meter: meter, WindowStart: next.GetWindowStart(), BillingAccountId: accountB,
			}
			nextReplay := proto.CloneOf(nextResult)
			nextReplay.Duplicate = true
			assertResponse(report(next, first, changed), currentStatus, nextResult, firstReplay, nextReplay)
			assertBilling(wantA, initialBalance-30, 6, 60)
		})
	}
}

func TestUnlinkGateBypassesNotificationAfterRelease(t *testing.T) {
	f := New(t, nil)
	gate := holdUnlinkGate(t, f)
	gate.Release()

	client := f.Client()
	const org = "synthetic-organization"
	wantCalls := cap(gate.Entered) + 1
	for call := 1; call <= wantCalls; call++ {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		_, err := client.UnlinkOrganization(ctx, connect.NewRequest(&accountsv1.UnlinkOrganizationRequest{OrganizationId: org}))
		cancel()
		if err != nil {
			t.Fatalf("unlink call %d after release: %v", call, err)
		}
	}
	if got := f.Calls(MethodUnlinkOrganization); got != wantCalls {
		t.Fatalf("unlink calls = %d, want %d", got, wantCalls)
	}
	if got := len(gate.Entered); got != 0 {
		t.Fatalf("notifications after release = %d, want 0", got)
	}
}

func TestUnlinkGateReleaseUnblocksFullNotificationBuffer(t *testing.T) {
	f := New(t, nil)
	gate := holdUnlinkGate(t, f)
	client := f.Client()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	wantCalls := cap(gate.Entered) + 1
	results := make(chan error, wantCalls)
	unlink := func() {
		_, err := client.UnlinkOrganization(ctx, connect.NewRequest(&accountsv1.UnlinkOrganizationRequest{OrganizationId: "synthetic-organization"}))
		results <- err
	}

	go unlink()
	awaitHeldUnlink(t, ctx, gate, results)
	gate.Entered <- struct{}{}

	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for range cap(gate.Entered) - 1 {
		go unlink()
	}
	for len(gate.Entered) < cap(gate.Entered) {
		select {
		case <-ticker.C:
		case err := <-results:
			t.Fatalf("unlink completed before release: %v", err)
		case <-ctx.Done():
			t.Fatal("timed out filling the gate notification buffer")
		}
	}

	go unlink()
	for f.Calls(MethodUnlinkOrganization) < wantCalls {
		select {
		case <-ticker.C:
		case err := <-results:
			t.Fatalf("unlink completed before release: %v", err)
		case <-ctx.Done():
			t.Fatal("timed out waiting for the extra unlink call to reach the gate")
		}
	}
	gate.Release()
	gate.Release()

	for call := 1; call <= wantCalls; call++ {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("unlink result %d after release: %v", call, err)
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for unlink result %d after release", call)
		}
	}
	if got := len(gate.Entered); got != cap(gate.Entered) {
		t.Fatalf("buffered notifications after release = %d, want %d", got, cap(gate.Entered))
	}
	if got := f.Calls(MethodUnlinkOrganization); got != wantCalls {
		t.Fatalf("unlink calls = %d, want %d", got, wantCalls)
	}
}

func TestUnlinkGateReturnsConfiguredFailureAfterRelease(t *testing.T) {
	f := New(t, nil)
	gate := holdUnlinkGate(t, f)
	const message = "synthetic unlink failure"
	f.Fail(MethodUnlinkOrganization, connect.NewError(connect.CodeUnavailable, errors.New(message)))

	client := f.Client()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	results := make(chan error, 1)
	go func() {
		_, err := client.UnlinkOrganization(ctx, connect.NewRequest(&accountsv1.UnlinkOrganizationRequest{OrganizationId: "synthetic-organization"}))
		results <- err
	}()
	awaitHeldUnlink(t, ctx, gate, results)
	gate.Release()

	select {
	case err := <-results:
		var rpcErr *connect.Error
		if !errors.As(err, &rpcErr) || rpcErr.Code() != connect.CodeUnavailable || rpcErr.Message() != message {
			t.Fatalf("unlink result = %v, want unavailable with message %q", err, message)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for the configured failure after release")
	}
	if got := f.Calls(MethodUnlinkOrganization); got != 1 {
		t.Fatalf("unlink calls = %d, want 1", got)
	}
}

func awaitHeldUnlink(t *testing.T, ctx context.Context, gate *Gate, results <-chan error) {
	t.Helper()
	select {
	case <-gate.Entered:
	case err := <-results:
		t.Fatalf("unlink completed before notifying the test: %v", err)
	case <-ctx.Done():
		t.Fatal("timed out waiting for the unlink notification")
	}
	select {
	case err := <-results:
		t.Fatalf("unlink completed before release: %v", err)
	default:
	}
}

func holdUnlinkGate(t *testing.T, f *Server) *Gate {
	t.Helper()
	gate := f.Hold(MethodUnlinkOrganization)
	t.Cleanup(func() {
		gate.Release()
		if !t.Failed() {
			return
		}

		// Keep draining until the server closes so a blocked send cannot hang cleanup.
		done := make(chan struct{})
		defer close(done)
		go func() {
			for {
				select {
				case <-gate.Entered:
				case <-done:
					return
				}
			}
		}()
		f.srv.Close()
	})
	return gate
}
