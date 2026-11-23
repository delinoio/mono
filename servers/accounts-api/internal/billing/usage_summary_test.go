// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"math"
	"slices"
	"testing"
	"time"

	"github.com/delinoio/mono/go/uuidv7"
)

// summaryFixture is an account with usage reports in one month.
type summaryFixture struct {
	*fixture
	acct   string
	linkID string
	from   time.Time
	to     time.Time
}

func newSummaryFixture(t *testing.T) *summaryFixture {
	t.Helper()
	f := newFixture(t)
	s := &summaryFixture{fixture: f, acct: f.account()}
	s.from = time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	s.to = s.from.AddDate(0, 1, 0)
	var err error
	if s.linkID, err = uuidv7.NewString(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Pool.Exec(ctx(), `INSERT INTO organization_links (organization_link_id, service, organization_id,
			billing_account_id, link_time) VALUES ($1, 'ownsand', 'linked', $2, $3)`, s.linkID, s.acct, s.from); err != nil {
		t.Fatal(err)
	}
	return s
}

// report commits a usage report charged to the account, in the window that
// starts at day of the month.
func (s *summaryFixture) report(org, meter string, day int, quantity, amount int64) {
	s.t.Helper()
	id, err := uuidv7.NewString()
	if err != nil {
		s.t.Fatal(err)
	}
	window := s.from.AddDate(0, 0, day-1)
	if _, err := s.db.Pool.Exec(ctx(), `INSERT INTO usage_reports (usage_report_id, service, organization_id, meter,
			window_start, quantity, amount_usd_micros, billing_account_id, organization_link_id, create_time)
		VALUES ($1, 'ownsand', $2, $3, $4, $5, $6, $7, $8, $4)`,
		id, org, meter, window, quantity, amount, s.acct, s.linkID); err != nil {
		s.t.Fatal(err)
	}
}

func (s *summaryFixture) summary(after *SummaryKey, limit int) ([]SummaryRow, int64) {
	s.t.Helper()
	rows, total, err := UsageSummary(ctx(), s.db.Pool, s.acct, s.from, s.to, after, limit)
	if err != nil {
		s.t.Fatal(err)
	}
	return rows, total
}

func summaryKeys(rows []SummaryRow) []string {
	out := []string{}
	for _, r := range rows {
		out = append(out, r.OrganizationID+"/"+r.Meter)
	}
	return out
}

func sumAmounts(rows []SummaryRow) int64 {
	var sum int64
	for _, r := range rows {
		sum += r.Amount
	}
	return sum
}

// A report committed between the reads of UsageSummary must show in both the
// total and the rows or in neither: one statement computes both, so both
// come from one snapshot. The querier commits the report after the first
// statement of UsageSummary ran, so that a read with several statements
// would count the report in the rows but not in the total.
func TestUsageSummaryIsOneSnapshot(t *testing.T) {
	s := newSummaryFixture(t)
	s.report("org-a", "ownsand.relay_egress", 1, 10, 3_000_000)
	s.report("org-b", "ownsand.relay_egress", 1, 20, 4_000_000)

	q := &interleavingQuerier{inner: s.db.Pool, afterFirst: func() {
		s.report("org-c", "ownsand.relay_egress", 2, 30, 10_000_000)
	}}
	rows, total, err := UsageSummary(ctx(), q, s.acct, s.from, s.to, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !q.done {
		t.Fatal("the commit did not run")
	}
	if want := []string{"org-a/ownsand.relay_egress", "org-b/ownsand.relay_egress"}; !slices.Equal(summaryKeys(rows), want) {
		t.Fatalf("rows during the commit = %v, want %v", summaryKeys(rows), want)
	}
	if total != 7_000_000 || sumAmounts(rows) != total {
		t.Fatalf("total %d with row amounts %d, want 7000000 for both", total, sumAmounts(rows))
	}

	rows, total = s.summary(nil, 10)
	if len(rows) != 3 || total != 17_000_000 || sumAmounts(rows) != total {
		t.Fatalf("after the commit: rows %v, total %d", summaryKeys(rows), total)
	}
}

// Pages cover the rows in byte order, the total covers the rows beyond the
// page, and an empty later page still returns the total.
func TestUsageSummaryPagesKeepTheTotal(t *testing.T) {
	s := newSummaryFixture(t)
	// "Zeta" sorts before "alpha" in byte order. Two reports of one group
	// are summed.
	s.report("alpha", "ownsand.relay_egress", 1, 1, 1_000_000)
	s.report("alpha", "ownsand.relay_egress", 2, 2, 2_000_000)
	s.report("alpha", "ownsand.r2_storage", 1, 4, 4_000_000)
	s.report("Zeta", "ownsand.relay_egress", 1, 8, 8_000_000)
	s.report("beta", "ownsand.relay_egress", 1, 16, 16_000_000)
	const total = 31_000_000

	rows, got := s.summary(nil, 2)
	if want := []string{"Zeta/ownsand.relay_egress", "alpha/ownsand.r2_storage"}; !slices.Equal(summaryKeys(rows), want) || got != total {
		t.Fatalf("first page = %v, total %d", summaryKeys(rows), got)
	}
	last := rows[len(rows)-1]
	rows, got = s.summary(&SummaryKey{Service: last.Service, OrganizationID: last.OrganizationID, Meter: last.Meter}, 2)
	if want := []string{"alpha/ownsand.relay_egress", "beta/ownsand.relay_egress"}; !slices.Equal(summaryKeys(rows), want) || got != total {
		t.Fatalf("second page = %v, total %d", summaryKeys(rows), got)
	}
	if rows[0].Quantity != 3 || rows[0].Amount != 3_000_000 {
		t.Fatalf("summed group = %+v", rows[0])
	}
	last = rows[len(rows)-1]
	rows, got = s.summary(&SummaryKey{Service: last.Service, OrganizationID: last.OrganizationID, Meter: last.Meter}, 2)
	if len(rows) != 0 || got != total {
		t.Fatalf("page after the last row = %v, total %d, want no rows and total %d", summaryKeys(rows), got, total)
	}
}

func TestUsageSummaryEmptyPeriod(t *testing.T) {
	s := newSummaryFixture(t)
	rows, total := s.summary(nil, 2)
	if rows == nil || len(rows) != 0 || total != 0 {
		t.Fatalf("empty period: rows %v, total %d", rows, total)
	}
	// A report in the first window after the period is not counted.
	s.report("org-a", "ownsand.relay_egress", 32, 1, 5_000_000)
	rows, total = s.summary(nil, 2)
	if len(rows) != 0 || total != 0 {
		t.Fatalf("report after the period: rows %v, total %d", summaryKeys(rows), total)
	}
	// A report inside the period is counted for its own account only. The
	// other account is queried over the same interval, so only the account
	// predicate (not the date bounds) can keep this row out of its summary.
	s.report("org-a", "ownsand.relay_egress", 1, 1, 1_000_000)
	rows, total = s.summary(nil, 2)
	if want := []SummaryRow{{"ownsand", "org-a", "ownsand.relay_egress", 1, 1_000_000}}; !slices.Equal(rows, want) || total != 1_000_000 {
		t.Fatalf("report in the period: rows %+v, total %d, want %+v and 1000000", rows, total, want)
	}
	other := s.account()
	otherRows, otherTotal, err := UsageSummary(ctx(), s.db.Pool, other, s.from, s.to, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if otherRows == nil || len(otherRows) != 0 || otherTotal != 0 {
		t.Fatalf("other account: rows %v, total %d", otherRows, otherTotal)
	}
}

// Sums above the int64 range saturate in the rows and in the total.
func TestUsageSummarySaturates(t *testing.T) {
	s := newSummaryFixture(t)
	s.report("org-a", "ownsand.relay_egress", 1, math.MaxInt64, math.MaxInt64)
	s.report("org-a", "ownsand.relay_egress", 2, math.MaxInt64, math.MaxInt64)
	s.report("org-b", "ownsand.relay_egress", 1, 5, math.MaxInt64-5)
	s.report("org-b", "ownsand.r2_storage", 1, 6, 5)
	rows, total := s.summary(nil, 10)
	if total != math.MaxInt64 {
		t.Fatalf("total = %d, want saturated", total)
	}
	want := []SummaryRow{
		{"ownsand", "org-a", "ownsand.relay_egress", math.MaxInt64, math.MaxInt64},
		{"ownsand", "org-b", "ownsand.r2_storage", 6, 5},
		{"ownsand", "org-b", "ownsand.relay_egress", 5, math.MaxInt64 - 5},
	}
	if !slices.Equal(rows, want) {
		t.Fatalf("rows = %+v, want %+v", rows, want)
	}
}
