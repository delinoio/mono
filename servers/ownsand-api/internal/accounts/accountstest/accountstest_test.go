// SPDX-License-Identifier: FSL-1.1-ALv2

package accountstest

import (
	"context"
	"errors"
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
