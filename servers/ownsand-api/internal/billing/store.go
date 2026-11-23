// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"context"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// exportedState is SNAPSHOT_EXPORT_STATE_EXPORTED as stored in snapshots.
const exportedState = int16(ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED)

// lockSealStateShared takes the seal state row in share mode and returns
// sealed_before. Counter writers take it so that a seal never runs between
// their check and their write.
func lockSealStateShared(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	var sealedBefore time.Time
	err := tx.QueryRow(ctx, "SELECT sealed_before FROM usage_seal_state WHERE singleton FOR SHARE").Scan(&sealedBefore)
	return sealedBefore.UTC(), err
}

// sortedKeys returns the keys in (organization, meter, window) order, so
// concurrent writers lock counter rows in the same order and never
// deadlock.
func sortedKeys(counts map[counterKey]int64) []counterKey {
	keys := make([]counterKey, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b counterKey) int {
		if c := strings.Compare(a.org, b.org); c != 0 {
			return c
		}
		if a.meter != b.meter {
			return int(a.meter) - int(b.meter)
		}
		switch {
		case a.window < b.window:
			return -1
		case a.window > b.window:
			return 1
		}
		return 0
	})
	return keys
}

// addCounters adds counts to the shared counter rows in tx. The caller holds
// the seal state row (in share or update mode) and passes sealedBefore,
// which is aligned to a window start. A count of a window that is already
// sealed moves to the oldest open window (sealedBefore), so it is neither
// lost nor added to a reported window; addCounters returns the original keys
// of the moved counts.
func addCounters(ctx context.Context, tx pgx.Tx, sealedBefore time.Time, counts map[counterKey]int64) ([]counterKey, error) {
	if len(counts) == 0 {
		return nil, nil
	}
	open := sealedBefore.Unix()
	var moved []counterKey
	merged := make(map[counterKey]int64, len(counts))
	for k, v := range counts {
		if k.window < open {
			moved = append(moved, k)
			k.window = open
		}
		merged[k] = saturatingAdd(merged[k], v)
	}
	keys := sortedKeys(merged)
	orgs := make([]string, len(keys))
	meters := make([]int16, len(keys))
	windows := make([]time.Time, len(keys))
	quantities := make([]int64, len(keys))
	for i, k := range keys {
		orgs[i], meters[i], windows[i], quantities[i] = k.org, int16(k.meter), k.windowStart(), merged[k]
	}
	// The sum saturates at the largest bigint instead of failing every later
	// flush with an overflow error.
	_, err := tx.Exec(ctx, `INSERT INTO usage_counters (organization_id, meter, window_start, quantity)
		SELECT o, m, w, q FROM unnest($1::uuid[], $2::smallint[], $3::timestamptz[], $4::bigint[]) WITH ORDINALITY AS t(o, m, w, q, n) ORDER BY n
		ON CONFLICT (organization_id, meter, window_start) DO UPDATE
		SET quantity = LEAST(usage_counters.quantity::numeric + EXCLUDED.quantity, 9223372036854775807)::bigint`,
		orgs, meters, windows, quantities)
	return moved, err
}

// insertFlushReceipt records the ID of a flush batch in the transaction that
// applies it and reports whether the receipt is new. A receipt that exists
// already means that an earlier transaction committed the batch, so the
// caller must not apply it again.
func insertFlushReceipt(ctx context.Context, tx pgx.Tx, batchID string, now time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, "INSERT INTO usage_flush_receipts (batch_id, create_time) VALUES ($1, $2) ON CONFLICT (batch_id) DO NOTHING", batchID, now)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// storageCounts returns the R2 storage byte-seconds of every organization
// with exported snapshots for one window: the part sizes of each snapshot in
// the export state EXPORTED, times the window length in seconds.
func storageCounts(ctx context.Context, tx pgx.Tx, window time.Time) (map[counterKey]int64, error) {
	rows, err := tx.Query(ctx, `SELECT s.organization_id::text, LEAST(sum(p.size_bytes), 9223372036854775807)::bigint
		FROM snapshots s JOIN snapshot_parts p ON p.snapshot_id = s.snapshot_id
		WHERE s.export_state = $1 GROUP BY s.organization_id`, exportedState)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[counterKey]int64{}
	for rows.Next() {
		var org string
		var stored int64
		if err := rows.Scan(&org, &stored); err != nil {
			return nil, err
		}
		if stored <= 0 {
			continue
		}
		byteSeconds := int64(math.MaxInt64)
		if stored <= math.MaxInt64/windowSeconds {
			byteSeconds = stored * windowSeconds
		}
		counts[counterKey{org: org, meter: ownsandv1.BillingMeter_BILLING_METER_R2_STORAGE, window: window.Unix()}] = byteSeconds
	}
	return counts, rows.Err()
}

// outboxRow is one sealed (organization, meter, window).
type outboxRow struct {
	org         string
	meter       ownsandv1.BillingMeter
	windowStart time.Time
	quantity    int64
	amount      int64
	attempts    int
}

// insertOutbox writes sealed rows and returns the keys that were already in
// the outbox (which the counter guard should make impossible).
func insertOutbox(ctx context.Context, tx pgx.Tx, rows []outboxRow, now time.Time) (map[counterKey]bool, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	orgs := make([]string, len(rows))
	meters := make([]int16, len(rows))
	windows := make([]time.Time, len(rows))
	quantities := make([]int64, len(rows))
	amounts := make([]int64, len(rows))
	for i, r := range rows {
		orgs[i], meters[i], windows[i], quantities[i], amounts[i] = r.org, int16(r.meter), r.windowStart, r.quantity, r.amount
	}
	res, err := tx.Query(ctx, `INSERT INTO usage_outbox (organization_id, meter, window_start, quantity, amount_usd_micros, seal_time, next_attempt_time)
		SELECT o, m, w, q, a, $6, $6 FROM unnest($1::uuid[], $2::smallint[], $3::timestamptz[], $4::bigint[], $5::bigint[]) AS t(o, m, w, q, a)
		ON CONFLICT (organization_id, meter, window_start) DO NOTHING
		RETURNING organization_id::text, meter, window_start`, orgs, meters, windows, quantities, amounts, now)
	if err != nil {
		return nil, err
	}
	inserted := map[counterKey]bool{}
	for res.Next() {
		var k counterKey
		var meter int16
		var w time.Time
		if err := res.Scan(&k.org, &meter, &w); err != nil {
			res.Close()
			return nil, err
		}
		k.meter, k.window = ownsandv1.BillingMeter(meter), w.Unix()
		inserted[k] = true
	}
	res.Close()
	if err := res.Err(); err != nil {
		return nil, err
	}
	conflicts := map[counterKey]bool{}
	for _, r := range rows {
		k := counterKey{org: r.org, meter: r.meter, window: r.windowStart.Unix()}
		if !inserted[k] {
			conflicts[k] = true
		}
	}
	return conflicts, nil
}

// supportedMeters returns the stored values of the OwnSand meters, the only
// meters that can be reported to delino-accounts (AccountsMeter maps every
// other value to METER_UNSPECIFIED, which accounts-api rejects for the whole
// request). The outbox queries of the reporter filter on it, so a row with
// another meter value neither fails nor delays the reports of supported rows.
func supportedMeters() []int16 {
	out := make([]int16, len(Meters))
	for i, m := range Meters {
		out[i] = int16(m)
	}
	return out
}

// dueRows returns up to limit unreported outbox rows of a supported meter
// whose next attempt is due, oldest window first. Rows of another meter value
// are skipped (and never modified), so they do not use up the limit.
func dueRows(ctx context.Context, q interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}, now time.Time, limit int) ([]outboxRow, error) {
	rows, err := q.Query(ctx, `SELECT organization_id::text, meter, window_start, quantity, amount_usd_micros, attempts FROM usage_outbox
		WHERE report_time IS NULL AND next_attempt_time <= $1 AND meter = ANY($3::smallint[])
		ORDER BY window_start, organization_id, meter LIMIT $2`, now, limit, supportedMeters())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		var meter int16
		if err := rows.Scan(&r.org, &meter, &r.windowStart, &r.quantity, &r.amount, &r.attempts); err != nil {
			return nil, err
		}
		r.meter, r.windowStart = ownsandv1.BillingMeter(meter), r.windowStart.UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// rowKeys returns the key arrays of rows for an unnest.
func rowKeys(rows []outboxRow) ([]string, []int16, []time.Time) {
	orgs := make([]string, len(rows))
	meters := make([]int16, len(rows))
	windows := make([]time.Time, len(rows))
	for i, r := range rows {
		orgs[i], meters[i], windows[i] = r.org, int16(r.meter), r.windowStart
	}
	return orgs, meters, windows
}
