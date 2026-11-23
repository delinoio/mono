// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"fmt"
	"testing"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/uuidv7"
	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/servers/accounts-api/internal/billing"
	"github.com/delinoio/mono/servers/accounts-api/internal/testenv"
)

// A usage_reports row whose stored meter key is not the canonical key of a
// meter ("ownsand_relay.egress" has its separator in the wrong place) is
// summarized as its own row with METER_UNSPECIFIED. It does not decode as the
// meter of the canonical key "ownsand.relay_egress", whose row keeps its
// meter. The row quantities, the row amounts and the account total are the
// stored values.
func TestListUsageSummaryMalformedStoredMeterKey(t *testing.T) {
	env := testenv.New(t)
	f := newFixture(t, env)
	subscribe(t, env, f)
	org := newOrg(t)
	link(t, env, f.AccountID, org, f.Owner.ID, "Linked")
	w, _ := sameMonthWindows(env)
	var linkID string
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT organization_link_id::text FROM organization_links WHERE organization_id = $1 AND unlink_time IS NULL`, org).Scan(&linkID); err != nil {
		t.Fatal(err)
	}
	canonical, _, _ := billing.MeterKey(meterEgress)
	insert := func(storedMeter string, quantity, amount int64) {
		t.Helper()
		id, err := uuidv7.NewString()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.DB.Pool.Exec(ctx(), `INSERT INTO usage_reports
			(usage_report_id, service, organization_id, meter, window_start, quantity, amount_usd_micros, billing_account_id, organization_link_id, create_time)
			VALUES ($1, 'ownsand', $2, $3, $4, $5, $6, $7, $8, now())`, id, org, storedMeter, w, quantity, amount, f.AccountID, linkID); err != nil {
			t.Fatal(err)
		}
	}
	insert(canonical, 7, 700)
	insert("ownsand_relay.egress", 11, 1_100)

	resp, err := env.BillingUsage(f.Viewer.ID).ListUsageSummary(ctx(), connect.NewRequest(&accountsv1.ListUsageSummaryRequest{
		BillingAccountId: f.AccountID, Year: int32(w.Year()), Month: int32(w.Month())}))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range resp.Msg.GetRows() {
		got[r.GetMeter().String()] = fmt.Sprintf("%t/%d/%d", r.GetOrganizationId() == org, r.GetQuantity(), r.GetAmountUsdMicros())
	}
	want := map[string]string{
		accountsv1.Meter_METER_OWNSAND_RELAY_EGRESS.String(): "true/7/700",
		accountsv1.Meter_METER_UNSPECIFIED.String():          "true/11/1100",
	}
	if len(resp.Msg.GetRows()) != 2 || len(got) != 2 || got[accountsv1.Meter_METER_OWNSAND_RELAY_EGRESS.String()] != want[accountsv1.Meter_METER_OWNSAND_RELAY_EGRESS.String()] ||
		got[accountsv1.Meter_METER_UNSPECIFIED.String()] != want[accountsv1.Meter_METER_UNSPECIFIED.String()] {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	if resp.Msg.GetTotalAmountUsdMicros() != 1_800 {
		t.Fatalf("total = %d, want 1800", resp.Msg.GetTotalAmountUsdMicros())
	}
}
