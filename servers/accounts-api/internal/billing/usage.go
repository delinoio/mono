// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/delinoio/mono/servers/accounts-api/internal/db"
)

// WindowLength is the length of a usage window.
const WindowLength = 5 * time.Minute

// Usage report results, which are also metric label values.
const (
	UsageCharged      = "charged"
	UsageDuplicate    = "duplicate"
	UsageUnattributed = "unattributed"
)

// UsageReport is one validated usage report with stable identifiers.
type UsageReport struct {
	Service        string
	OrganizationID string
	Meter          string
	WindowStart    time.Time
	Quantity       int64
	Amount         int64
}

// UsageResult is the first result of a usage report key.
type UsageResult struct {
	Duplicate        bool
	Unattributed     bool
	BillingAccountID string
	LedgerEntryID    string
}

// storedReport is a usage_reports row.
type storedReport struct {
	Quantity         int64
	Amount           int64
	BillingAccountID *string
	LedgerEntryID    *string
}

func (r storedReport) result() UsageResult {
	res := UsageResult{Duplicate: true, Unattributed: r.BillingAccountID == nil}
	if r.BillingAccountID != nil {
		res.BillingAccountID = *r.BillingAccountID
	}
	if r.LedgerEntryID != nil {
		res.LedgerEntryID = *r.LedgerEntryID
	}
	return res
}

func loadReport(ctx context.Context, q db.Querier, r UsageReport) (storedReport, bool, error) {
	var st storedReport
	err := q.QueryRow(ctx, `SELECT quantity, amount_usd_micros, billing_account_id::text, ledger_entry_id::text
		FROM usage_reports WHERE service = $1 AND organization_id = $2 AND meter = $3 AND window_start = $4`,
		r.Service, r.OrganizationID, r.Meter, r.WindowStart).Scan(&st.Quantity, &st.Amount, &st.BillingAccountID, &st.LedgerEntryID)
	if errors.Is(err, pgx.ErrNoRows) {
		return storedReport{}, false, nil
	}
	return st, err == nil, err
}

// attribution is the link that a report is charged to.
type attribution struct {
	LinkID    string
	AccountID string
}

// attribute returns the link of the organization that overlapped the window
// [start, start + 5 minutes): a link from link_time until unlink_time (open
// while current). An empty link, whose unlink_time equals its link_time (a
// link that was unlinked at the instant it started), covers no time and never
// overlaps a window. When several links overlapped, the one with the latest
// link_time wins, even when every overlapping link ended before the end of
// the window. ok is false without such a link.
func attribute(ctx context.Context, q db.Querier, r UsageReport) (attribution, bool, error) {
	var a attribution
	err := q.QueryRow(ctx, `SELECT organization_link_id::text, billing_account_id::text FROM organization_links
		WHERE service = $1 AND organization_id = $2 AND link_time < $4 AND (unlink_time IS NULL OR unlink_time > $3)
		AND (unlink_time IS NULL OR unlink_time > link_time)
		ORDER BY link_time DESC, organization_link_id DESC LIMIT 1`,
		r.Service, r.OrganizationID, r.WindowStart, r.WindowStart.Add(WindowLength)).Scan(&a.LinkID, &a.AccountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return attribution{}, false, nil
	}
	return a, err == nil, err
}

// LockOrganization takes a transaction-scoped advisory lock on one
// organization of a service. It serializes the creation of a link of the
// organization with every report of the organization, attributed or not,
// whose last attribution read and insert would otherwise miss a link that
// commits in between (a link to another billing account, for a report that
// an earlier link attributed). The lock is held until tx ends.
//
// Lock order: a transaction that takes a billing account lock takes it
// first and this lock second: a new link and an attributed report both do.
// The unattributed report path takes only this lock and no billing account
// lock, and an unlink takes only the billing account lock, so no two
// orders form a cycle. An unattributed report that finds a link after it
// holds this lock rolls back, which releases the lock, and retries; the
// retry locks the billing account of the link first.
func LockOrganization(ctx context.Context, tx pgx.Tx, service, organizationID string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('usage_attr:' || $1::text || ':' || $2::text, 0))`,
		service, organizationID)
	return err
}

// errAttributionChanged makes ReportUsage retry when a link changed between
// the attribution and the billing account lock.
var errAttributionChanged = errors.New("the attribution of the usage report changed")

// ReportUsage applies one usage report. A repeated key returns the first
// result with Duplicate set and deducts nothing again; other values than
// the first ones are logged as a mismatch. A report is charged to the
// billing account of the link that overlapped its window, through Deduct,
// which can make the balance negative; without such a link it is stored
// as unattributed usage.
//
// Concurrency: the attributed path locks the billing account, then the
// organization (see LockOrganization), and reads the attribution again
// before it claims the key. Concurrent reports of one billing account
// serialize, a new link of the organization waits until the report
// commits, and a concurrent report with the same key waits for the first
// one and then reads its result. A report whose attribution changed before
// it held both locks rolls back and retries, so the stored attribution,
// the result and the deduction agree.
func (s *Service) ReportUsage(ctx context.Context, r UsageReport) (UsageResult, error) {
	r.WindowStart = r.WindowStart.UTC()
	if st, ok, err := loadReport(ctx, s.db.Pool, r); err != nil || ok {
		if ok {
			s.duplicate(ctx, r, st)
			return st.result(), nil
		}
		return UsageResult{}, err
	}
	for attempt := 0; ; attempt++ {
		res, queued, err := s.applyReport(ctx, r)
		if errors.Is(err, errAttributionChanged) && attempt < 3 {
			continue
		}
		if err != nil {
			return UsageResult{}, err
		}
		if queued {
			s.Wake()
		}
		return res, nil
	}
}

func (s *Service) applyReport(ctx context.Context, r UsageReport) (UsageResult, bool, error) {
	link, attributed, err := attribute(ctx, s.db.Pool, r)
	if err != nil {
		return UsageResult{}, false, err
	}
	var res UsageResult
	var queued bool
	var dup *storedReport
	err = s.db.InTx(ctx, func(tx pgx.Tx) error {
		res, queued, dup = UsageResult{}, false, nil
		now := s.timestamp()
		if attributed {
			if _, err := lockAccount(ctx, tx, link.AccountID); err != nil {
				return err
			}
			// A link or unlink of the organization locks the billing account
			// too; read the attribution again under the lock. A new link to
			// another billing account does not wait for the lock above, so
			// take the organization lock as well (after the billing account
			// lock, like a new link) and hold both to the commit: a link
			// that commits before the lock is seen by the read below, and a
			// link that waits for the lock inserts after the report, whose
			// stored attribution then stands.
			if err := LockOrganization(ctx, tx, r.Service, r.OrganizationID); err != nil {
				return err
			}
			again, ok, err := attribute(ctx, tx, r)
			if err != nil {
				return err
			}
			if !ok || again != link {
				return errAttributionChanged
			}
		} else {
			// Serialize with the creation of a link of the organization: a
			// link that commits before this lock is seen by the read below,
			// and a link that waits for this lock inserts after the
			// unattributed row. The lock is held to the commit.
			if err := LockOrganization(ctx, tx, r.Service, r.OrganizationID); err != nil {
				return err
			}
			if _, ok, err := attribute(ctx, tx, r); err != nil || ok {
				if ok {
					return errAttributionChanged
				}
				return err
			}
		}
		id, err := newID()
		if err != nil {
			return err
		}
		var accountID, linkID *string
		if attributed {
			accountID, linkID = &link.AccountID, &link.LinkID
		}
		tag, err := tx.Exec(ctx, `INSERT INTO usage_reports (usage_report_id, service, organization_id, meter, window_start,
				quantity, amount_usd_micros, billing_account_id, organization_link_id, create_time)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			ON CONFLICT (service, organization_id, meter, window_start) DO NOTHING`,
			id, r.Service, r.OrganizationID, r.Meter, r.WindowStart, r.Quantity, r.Amount, accountID, linkID, now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			// A concurrent report with the same key committed first.
			st, ok, err := loadReport(ctx, tx, r)
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("usage report key conflict without a row")
			}
			dup = &st
			return nil
		}
		if !attributed {
			res = UsageResult{Unattributed: true}
			return nil
		}
		d, err := s.Deduct(ctx, tx, Deduction{
			BillingAccountID: link.AccountID, AmountUSDMicros: r.Amount, Service: r.Service,
			OrganizationID: r.OrganizationID, Meter: r.Meter, WindowStart: r.WindowStart, Quantity: r.Quantity,
		})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE usage_reports SET ledger_entry_id = $2 WHERE usage_report_id = $1`,
			id, d.LedgerEntryID); err != nil {
			return err
		}
		res = UsageResult{BillingAccountID: link.AccountID, LedgerEntryID: d.LedgerEntryID}
		queued = d.AutoTopUpQueued
		return nil
	})
	if err != nil {
		return UsageResult{}, false, err
	}
	if dup != nil {
		s.duplicate(ctx, r, *dup)
		return dup.result(), false, nil
	}
	result := UsageCharged
	if res.Unattributed {
		result = UsageUnattributed
	}
	s.metrics.UsageReport(r.Service, result)
	s.logger.LogAttrs(ctx, slog.LevelInfo, "billing_usage_reported",
		slog.String("service", r.Service),
		slog.String("organization_id", r.OrganizationID),
		slog.String("meter", r.Meter),
		slog.Time("window_start", r.WindowStart),
		slog.Int64("quantity", r.Quantity),
		slog.Int64("amount_usd_micros", r.Amount),
		slog.String("result", result),
		slog.String("billing_account_id", res.BillingAccountID),
		slog.String("ledger_entry_id", res.LedgerEntryID))
	return res, queued, nil
}

// duplicate counts and logs a repeated key, and logs a mismatch when the
// values differ from the first report, whose values are kept.
func (s *Service) duplicate(ctx context.Context, r UsageReport, first storedReport) {
	s.metrics.UsageReport(r.Service, UsageDuplicate)
	attrs := []slog.Attr{
		slog.String("service", r.Service),
		slog.String("organization_id", r.OrganizationID),
		slog.String("meter", r.Meter),
		slog.Time("window_start", r.WindowStart),
	}
	if first.Quantity != r.Quantity || first.Amount != r.Amount {
		s.logger.LogAttrs(ctx, slog.LevelWarn, "billing_usage_report_mismatch", append(attrs,
			slog.Int64("first_quantity", first.Quantity),
			slog.Int64("quantity", r.Quantity),
			slog.Int64("first_amount_usd_micros", first.Amount),
			slog.Int64("amount_usd_micros", r.Amount))...)
		return
	}
	s.logger.LogAttrs(ctx, slog.LevelDebug, "billing_usage_report_duplicate", attrs...)
}

// satCeiling is the largest value that a monthly usage sum returns. Postgres
// sums bigint columns as numeric, so a sum of stored rows can exceed the int64
// range that the API fields use; the read queries saturate such a sum here
// instead of failing the cast. The stored rows stay exact.
var satCeiling = strconv.FormatInt(math.MaxInt64, 10)

// saturatedSum returns the SQL expression that sums a bigint column over the
// rows of a group (0 without rows) as a bigint that saturates at
// math.MaxInt64. Both usage columns are constrained to be non-negative, so
// the sum needs no lower bound.
func saturatedSum(column string) string {
	return "LEAST(COALESCE(SUM(" + column + "), 0), " + satCeiling + ")::bigint"
}

// MeterUsage is the usage of one meter in a period. Quantity and Amount
// saturate at math.MaxInt64.
type MeterUsage struct {
	Meter    string
	Quantity int64
	Amount   int64
}

// OrganizationMonthUsage returns the usage of every report of the
// organization (charged to any billing account or unattributed) in windows
// that start in [from, to), by meter.
func OrganizationMonthUsage(ctx context.Context, q db.Querier, service, organizationID string, from, to time.Time) (map[string]MeterUsage, error) {
	rows, err := q.Query(ctx, `SELECT meter, `+saturatedSum("quantity")+`, `+saturatedSum("amount_usd_micros")+`
		FROM usage_reports WHERE service = $1 AND organization_id = $2 AND window_start >= $3 AND window_start < $4
		GROUP BY meter`, service, organizationID, from, to)
	if err != nil {
		return nil, err
	}
	usages, err := pgx.CollectRows(rows, pgx.RowToStructByPos[MeterUsage])
	if err != nil {
		return nil, err
	}
	out := make(map[string]MeterUsage, len(usages))
	for _, u := range usages {
		out[u.Meter] = u
	}
	return out, nil
}

// SummaryRow is the usage of one organization and meter that was charged
// to a billing account in a period. Quantity and Amount saturate at
// math.MaxInt64.
type SummaryRow struct {
	Service        string
	OrganizationID string
	Meter          string
	Quantity       int64
	Amount         int64
}

// SummaryKey is the position of a summary row: rows are ordered by service,
// organization ID and meter identifier in byte order.
type SummaryKey struct {
	Service        string
	OrganizationID string
	Meter          string
}

// UsageSummary returns up to limit rows of the usage charged to the billing
// account in windows that start in [from, to), after the key (from the
// start when nil), and the total amount of every row of the period. The
// total and the sums of the rows saturate at math.MaxInt64.
//
// One statement computes the total and the page, so both come from the same
// snapshot of committed reports even when q is a pool: separate statements
// would each take their own snapshot, and a report committed between them
// would show in one but not the other. The total is the root of a LEFT JOIN
// to the page, so a page without rows (an empty period or a page after the
// last row) still returns the total, with NULL row columns.
func UsageSummary(ctx context.Context, q db.Querier, accountID string, from, to time.Time, after *SummaryKey, limit int) ([]SummaryRow, int64, error) {
	var k SummaryKey
	if after != nil {
		k = *after
	}
	rows, err := q.Query(ctx, `WITH base AS NOT MATERIALIZED (
			SELECT service, organization_id, meter, quantity, amount_usd_micros FROM usage_reports
			WHERE billing_account_id = $1 AND window_start >= $2 AND window_start < $3
		), tot AS (
			SELECT `+saturatedSum("amount_usd_micros")+` AS total FROM base
		), page AS (
			SELECT service, organization_id, meter, `+saturatedSum("quantity")+` AS quantity, `+saturatedSum("amount_usd_micros")+` AS amount
			FROM base
			WHERE ($4 = false OR (service COLLATE "C", organization_id COLLATE "C", meter COLLATE "C") > ($5, $6, $7))
			GROUP BY service, organization_id, meter
			ORDER BY service COLLATE "C", organization_id COLLATE "C", meter COLLATE "C"
			LIMIT $8
		)
		SELECT tot.total, page.service, page.organization_id, page.meter, page.quantity, page.amount
		FROM tot LEFT JOIN page ON true
		ORDER BY page.service COLLATE "C", page.organization_id COLLATE "C", page.meter COLLATE "C"`,
		accountID, from, to, after != nil, k.Service, k.OrganizationID, k.Meter, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var (
		total int64
		out   = []SummaryRow{}
	)
	for rows.Next() {
		var (
			service, organizationID, meter *string
			quantity, amount               *int64
		)
		if err := rows.Scan(&total, &service, &organizationID, &meter, &quantity, &amount); err != nil {
			return nil, 0, err
		}
		if service == nil {
			// No row on the page: this row only carries the total.
			continue
		}
		out = append(out, SummaryRow{Service: *service, OrganizationID: *organizationID, Meter: *meter, Quantity: *quantity, Amount: *amount})
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}
