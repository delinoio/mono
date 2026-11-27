// SPDX-License-Identifier: FSL-1.1-ALv2

package accountstest

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
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
			var elapsed atomic.Int64
			f := New(t, func() time.Time { return start.Add(time.Duration(elapsed.Load())) })
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
				report, err := client.ReportUsage(ctx, connect.NewRequest(&accountsv1.ReportUsageRequest{Reports: []*accountsv1.UsageReport{{
					Service: service, OrganizationId: orgs[0], Meter: accountsv1.Meter_METER_OWNSAND_CLASS_A_RPC,
					WindowStart: timestamppb.New(start.Add(c.windowOffset)), Quantity: 1, AmountUsdMicros: c.amount,
				}}}))
				if err != nil {
					t.Fatal(err)
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
