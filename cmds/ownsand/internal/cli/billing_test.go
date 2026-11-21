// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/cmds/ownsand/internal/credstore"
	ownsand "github.com/delinoio/mono/go/ownsand"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

// fakeBilling serves BillingService with the access rules of the control
// plane: the Authorization header selects the caller (the API key, or a
// stored user token named after the caller's role).
type fakeBilling struct {
	ownsandv1connect.UnimplementedBillingServiceHandler
	f *fakeServer
}

func callerRole(h interface{ Get(string) string }) string {
	switch h.Get("Authorization") {
	case "Bearer owner-token":
		return "owner"
	case "Bearer admin-token":
		return "admin"
	case "Bearer member-token":
		return "member"
	}
	return "api-key"
}

func (h *fakeBilling) GetBillingStatus(_ context.Context, r *connect.Request[ownsandv1.GetBillingStatusRequest]) (*connect.Response[ownsandv1.GetBillingStatusResponse], error) {
	if r.Msg.GetOrganizationId() != h.f.org {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("organization not found"))
	}
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	return connect.NewResponse(&ownsandv1.GetBillingStatusResponse{Billing: h.f.billing}), nil
}

func (h *fakeBilling) GetBillingUsage(_ context.Context, r *connect.Request[ownsandv1.GetBillingUsageRequest]) (*connect.Response[ownsandv1.GetBillingUsageResponse], error) {
	if r.Msg.GetOrganizationId() != h.f.org {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("organization not found"))
	}
	if role := callerRole(r.Header()); role != "owner" && role != "admin" {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("owner or admin only"))
	}
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	if !h.f.billing.GetLinked() {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("no linked billing account"))
	}
	return connect.NewResponse(h.f.usage), nil
}

const billingMsg = "the organization is suspended because of non-payment; manage billing at https://accounts.delino.io"

func cliBillingErr(reason ownsandv1.BillingReason) error {
	err := connect.NewError(connect.CodeResourceExhausted, errors.New(billingMsg))
	d, derr := connect.NewErrorDetail(&ownsandv1.ErrorDetail{Code: ownsandv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED, Message: billingMsg, BillingReason: reason})
	if derr != nil {
		panic(derr)
	}
	err.AddDetail(d)
	return err
}

var (
	billingStart  = time.Date(2026, 11, 2, 3, 4, 5, 0, time.UTC)
	billingMonth  = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	billingAcctID = "0192f4b6-0000-7000-8000-0000000000bb"
)

func billingUsage() *ownsandv1.GetBillingUsageResponse {
	return &ownsandv1.GetBillingUsageResponse{
		BillingAccountId:   billingAcctID,
		BillingAccountName: "Acme billing",
		BalanceUsdMicros:   12_345_678,
		MonthStart:         timestamppb.New(billingMonth),
		MeterUsages: []*ownsandv1.BillingMeterUsage{
			{Meter: ownsandv1.BillingMeter_BILLING_METER_RELAY_EGRESS, Quantity: 3 << 29, AmountUsdMicros: 135_000},
			{Meter: ownsandv1.BillingMeter_BILLING_METER_R2_STORAGE, Quantity: (1 << 28) * 730 * 3600, AmountUsdMicros: 7_500},
			{Meter: ownsandv1.BillingMeter_BILLING_METER_CLASS_A_RPC, Quantity: 1234, AmountUsdMicros: 5_553},
			{Meter: ownsandv1.BillingMeter_BILLING_METER_CLASS_B_RPC, Quantity: 10, AmountUsdMicros: 4},
		},
	}
}

func linkedAllowed() *ownsandv1.OrganizationBilling {
	return &ownsandv1.OrganizationBilling{Phase: ownsandv1.BillingPhase_BILLING_PHASE_ALLOWED, Linked: true}
}

func unlinkedBlocked() *ownsandv1.OrganizationBilling {
	return &ownsandv1.OrganizationBilling{
		Phase:        ownsandv1.BillingPhase_BILLING_PHASE_BLOCKED,
		Reason:       ownsandv1.BillingReason_BILLING_REASON_ACCOUNT_REQUIRED,
		BlockedSince: timestamppb.New(billingStart),
		SuspendTime:  timestamppb.New(billingStart.Add(72 * time.Hour)),
	}
}

// signInAs replaces the API key with a stored user token for role.
func (r *runner) signInAs(role string) {
	r.t.Helper()
	if role == "api-key" {
		return
	}
	delete(r.env, ownsand.EnvAPIKey)
	st := credstore.Store{Dir: r.env[EnvConfigDir]}
	if err := st.Save(&credstore.Credentials{RefreshToken: "r", AccessToken: role + "-token", AccessTokenExpiry: time.Now().Add(time.Hour)}); err != nil {
		r.t.Fatal(err)
	}
}

func TestBillingStatus(t *testing.T) {
	for _, role := range []string{"owner", "admin", "member", "api-key"} {
		full := role == "owner" || role == "admin"
		t.Run(role+" linked text", func(t *testing.T) {
			r := newRunner(t)
			r.signInAs(role)
			r.f.billing, r.f.usage = linkedAllowed(), billingUsage()
			res := r.run("billing", "status", "--no-color")
			if res.code != ExitOK {
				t.Fatalf("%+v", res)
			}
			for _, want := range []string{"organization: " + r.f.org, "status: allowed"} {
				if !strings.Contains(res.stdout, want) {
					t.Fatalf("stdout misses %q:\n%s", want, res.stdout)
				}
			}
			usageLines := []string{
				"billing account: Acme billing (" + billingAcctID + ")",
				"balance: $12.35",
				"month start: 2026-11-01T00:00:00Z",
				"relay egress", "1.50 GiB", "$0.14",
				"R2 storage", "0.25 GiB-months", "$0.0075",
				"Class A RPCs", "1234 calls", "$0.005553",
				"Class B RPCs", "10 calls", "$0.000004",
			}
			for _, want := range usageLines {
				if full != strings.Contains(res.stdout, want) {
					t.Fatalf("%s: stdout contains %q = %v, want %v:\n%s", role, want, !full, full, res.stdout)
				}
			}
			if !full && !strings.Contains(res.stdout, "billing account: linked") {
				t.Fatalf("stdout:\n%s", res.stdout)
			}
			calls := strings.Join(r.f.callNames(), ",")
			if role == "api-key" && strings.Contains(calls, "GetBillingUsage") {
				t.Fatalf("an API key must not call GetBillingUsage: %s", calls)
			}
		})
		t.Run(role+" linked json", func(t *testing.T) {
			r := newRunner(t)
			r.signInAs(role)
			r.f.billing, r.f.usage = linkedAllowed(), billingUsage()
			res := r.run("billing", "status", "--format", "json")
			if res.code != ExitOK {
				t.Fatalf("%+v", res)
			}
			var out struct {
				Status struct {
					Billing struct {
						Phase  string `json:"phase"`
						Linked bool   `json:"linked"`
					} `json:"billing"`
				} `json:"status"`
				Usage *struct {
					BillingAccountID string `json:"billing_account_id"`
					Balance          string `json:"balance_usd_micros"`
					MeterUsages      []struct {
						Meter  string `json:"meter"`
						Amount string `json:"amount_usd_micros"`
					} `json:"meter_usages"`
				} `json:"usage"`
			}
			if err := json.Unmarshal([]byte(res.stdout), &out); err != nil {
				t.Fatalf("%v: %q", err, res.stdout)
			}
			if out.Status.Billing.Phase != "BILLING_PHASE_ALLOWED" || !out.Status.Billing.Linked {
				t.Fatalf("status %+v", out.Status)
			}
			if full {
				if out.Usage == nil || out.Usage.Balance != "12345678" || out.Usage.BillingAccountID != billingAcctID || len(out.Usage.MeterUsages) != 4 || out.Usage.MeterUsages[3].Amount != "4" {
					t.Fatalf("usage %+v", out.Usage)
				}
			} else if out.Usage != nil {
				t.Fatalf("%s must see the status only, got usage %+v", role, out.Usage)
			}
		})
		t.Run(role+" not linked", func(t *testing.T) {
			r := newRunner(t)
			r.signInAs(role)
			r.f.billing = unlinkedBlocked()
			res := r.run("billing", "status", "--no-color")
			if res.code != ExitOK {
				t.Fatalf("%+v", res)
			}
			for _, want := range []string{
				"status: blocked", "reason: account-required", "billing account: not linked",
				"blocked since: 2026-11-02T03:04:05Z", "suspension time: 2026-11-05T03:04:05Z",
				"organization settings page", "https://accounts.delino.io",
			} {
				if !strings.Contains(res.stdout, want) {
					t.Fatalf("stdout misses %q:\n%s", want, res.stdout)
				}
			}
			if strings.Contains(strings.Join(r.f.callNames(), ","), "GetBillingUsage") {
				t.Fatal("an organization without a link must not call GetBillingUsage")
			}
			res = r.run("billing", "status", "--format", "json")
			if res.code != ExitOK || !json.Valid([]byte(res.stdout)) || strings.Contains(res.stdout, `"usage"`) {
				t.Fatalf("%+v", res)
			}
			if !strings.Contains(res.stderr, "https://accounts.delino.io") {
				t.Fatalf("the JSON mode note goes to standard error: %+v", res)
			}
		})
	}
}

func TestBillingStatusSuspended(t *testing.T) {
	r := newRunner(t)
	r.signInAs("owner")
	r.f.billing = &ownsandv1.OrganizationBilling{
		Phase:        ownsandv1.BillingPhase_BILLING_PHASE_SUSPENDED,
		Reason:       ownsandv1.BillingReason_BILLING_REASON_SUSPENDED,
		Linked:       true,
		BlockedSince: timestamppb.New(billingStart),
		SuspendTime:  timestamppb.New(billingStart.Add(72 * time.Hour)),
	}
	u := billingUsage()
	u.BalanceUsdMicros = -1_234_567
	r.f.usage = u
	res := r.run("billing", "status", "--no-color")
	if res.code != ExitOK {
		t.Fatalf("%+v", res)
	}
	for _, want := range []string{"status: suspended", "reason: suspended", "suspended since: 2026-11-05T03:04:05Z", "balance: -$1.23", "running sandboxes are paused", "https://accounts.delino.io"} {
		if !strings.Contains(res.stdout, want) {
			t.Fatalf("stdout misses %q:\n%s", want, res.stdout)
		}
	}
}

// A command that billing enforcement rejects prints the server message with
// the billing link, exits with the contract status and is not retried.
func TestBillingRejectedCommands(t *testing.T) {
	reasons := []ownsandv1.BillingReason{
		ownsandv1.BillingReason_BILLING_REASON_ACCOUNT_REQUIRED,
		ownsandv1.BillingReason_BILLING_REASON_BLOCKED,
		ownsandv1.BillingReason_BILLING_REASON_SUSPENDED,
	}
	count := func(r *runner, name string) int {
		n := 0
		for _, c := range r.f.callNames() {
			if c == name {
				n++
			}
		}
		return n
	}
	for _, reason := range reasons {
		t.Run(reason.String(), func(t *testing.T) {
			r := newRunner(t)
			r.f.reject = cliBillingErr(reason)

			res := r.run("sandbox", "create", "--image", "linux", "--idle-ttl", "10m", "--max-ttl", "1h", "--vcpu", "1", "--memory", "512", "--disk", "5")
			if res.code != ExitError || !strings.Contains(res.stderr, billingMsg) || res.stdout != "" {
				t.Fatalf("sandbox create: %+v", res)
			}
			if n := count(r, "CreateSandbox"); n != 1 {
				t.Fatalf("%d CreateSandbox calls, want 1", n)
			}

			res = r.run("sandbox", "exec", r.f.sb, "--", "echo", "hi")
			if res.code != ExitDataPath || !strings.Contains(res.stderr, "data path: billing: "+billingMsg) {
				t.Fatalf("sandbox exec: %+v", res)
			}
			res = r.run("--format", "json", "sandbox", "exec", r.f.sb, "--", "echo", "hi")
			if res.code != ExitDataPath || !strings.Contains(res.stdout, `"event":"data_path_error"`) || !strings.Contains(res.stdout, "https://accounts.delino.io") {
				t.Fatalf("sandbox exec json: %+v", res)
			}

			res = r.run("file", "read", r.f.sb, "/etc/hosts")
			if res.code != ExitError || !strings.Contains(res.stderr, billingMsg) {
				t.Fatalf("file read: %+v", res)
			}
			// One AuthorizeRelay for each of the three data path commands.
			if n := count(r, "AuthorizeRelay"); n != 3 {
				t.Fatalf("%d AuthorizeRelay calls, want 3 (no retry)", n)
			}
		})
	}
}

func TestBillingErrorTextAddsMissingLink(t *testing.T) {
	err := connect.NewError(connect.CodeResourceExhausted, errors.New("blocked"))
	d, _ := connect.NewErrorDetail(&ownsandv1.ErrorDetail{BillingReason: ownsandv1.BillingReason_BILLING_REASON_BLOCKED})
	err.AddDetail(d)
	if got := describeError(err); got != "billing: blocked; manage billing at https://accounts.delino.io" {
		t.Fatalf("describeError = %q", got)
	}
	capacity := connect.NewError(connect.CodeResourceExhausted, errors.New("agent at capacity"))
	if got := describeError(capacity); got != "resource_exhausted: agent at capacity" {
		t.Fatalf("describeError = %q", got)
	}
}

func TestBillingAmountFormat(t *testing.T) {
	for _, c := range []struct {
		micros int64
		usd    string
		cost   string
	}{
		{0, "$0.00", "$0.00"},
		{1, "$0.00", "$0.000001"},
		{4_999, "$0.00", "$0.004999"},
		{5_000, "$0.01", "$0.005"},
		{7_500, "$0.01", "$0.0075"},
		{9_999, "$0.01", "$0.009999"},
		{10_000, "$0.01", "$0.01"},
		{14_999, "$0.01", "$0.01"},
		{15_000, "$0.02", "$0.02"},
		{12_345_678, "$12.35", "$12.35"},
		{-4_999, "$0.00", "-$0.004999"},
		{-5_000, "-$0.01", "-$0.005"},
		{-1_234_567, "-$1.23", "-$1.23"},
		{9_223_372_036_854_775_807, "$9223372036854.78", "$9223372036854.78"},
	} {
		if got := formatUSD(c.micros); got != c.usd {
			t.Errorf("formatUSD(%d) = %q, want %q", c.micros, got, c.usd)
		}
		if got := formatUsageCost(c.micros); got != c.cost {
			t.Errorf("formatUsageCost(%d) = %q, want %q", c.micros, got, c.cost)
		}
	}
	if got := formatDecimal(big.NewRat(1, 1<<40), true); got != "0.00" {
		t.Errorf("a tiny quantity = %q", got)
	}
}
