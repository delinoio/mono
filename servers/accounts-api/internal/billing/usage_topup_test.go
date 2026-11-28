// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestUsageLogsAutoTopUpAfterCommit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		createFault string
		dropFault   string
	}{
		{
			name: "callback_error",
			createFault: `CREATE TRIGGER fail_usage BEFORE UPDATE OF ledger_entry_id ON usage_reports
				FOR EACH ROW EXECUTE FUNCTION fail_usage()`,
			dropFault: `DROP TRIGGER fail_usage ON usage_reports`,
		},
		{
			name: "commit_error",
			createFault: `CREATE CONSTRAINT TRIGGER fail_usage AFTER INSERT ON top_ups
				DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_usage()`,
			dropFault: `DROP TRIGGER fail_usage ON top_ups`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			accountID := f.account()
			f.activate(accountID)
			f.grant(accountID, LedgerManualTopUp, 30*usd)
			f.enableAutoTopUp(accountID, 10*usd, 25*usd, 50*usd)
			report := UsageReport{
				Service: "ownsand", OrganizationID: f.link(accountID, "Test organization"),
				Meter: "ownsand.relay_egress", WindowStart: f.clock.Now().Truncate(WindowLength),
				Quantity: 1, Amount: 25 * usd,
			}

			var logs bytes.Buffer
			f.s.logger = slog.New(slog.NewJSONHandler(&logs, nil))
			type event struct {
				Message     string `json:"msg"`
				Level       string `json:"level"`
				AccountID   string `json:"billing_account_id"`
				TopUpID     string `json:"top_up_id"`
				Amount      int64  `json:"amount_usd_micros"`
				Balance     int64  `json:"balance_usd_micros"`
				MonthToDate *int64 `json:"month_to_date_usd_micros"`
				Reason      string `json:"reason"`
			}
			readEvents := func() (queued, skipped []event) {
				t.Helper()
				decoder := json.NewDecoder(&logs)
				for {
					var e event
					if err := decoder.Decode(&e); err != nil {
						if errors.Is(err, io.EOF) {
							return queued, skipped
						}
						t.Fatal(err)
					}
					switch e.Message {
					case "billing_auto_top_up_queued":
						queued = append(queued, e)
					case "billing_auto_top_up_skipped":
						skipped = append(skipped, e)
					}
				}
			}
			checkWake := func(want bool) {
				t.Helper()
				select {
				case <-f.s.wake:
					if !want {
						t.Error("unexpected worker wake")
					}
				default:
					if want {
						t.Error("missing worker wake")
					}
				}
			}
			checkState := func(balance int64, reports, topUps int) {
				t.Helper()
				if got := f.balance(accountID); got != balance {
					t.Errorf("balance = %d, want %d", got, balance)
				}
				var gotReports, gotTopUps int
				if err := f.db.Pool.QueryRow(ctx(), `SELECT (SELECT count(*) FROM usage_reports),
					(SELECT count(*) FROM top_ups WHERE kind = 'auto')`).Scan(&gotReports, &gotTopUps); err != nil {
					t.Fatal(err)
				}
				if gotReports != reports || gotTopUps != topUps {
					t.Errorf("report and top-up counts = %d, %d, want %d, %d", gotReports, gotTopUps, reports, topUps)
				}
				var deductions int
				for _, entry := range f.ledger(accountID) {
					if entry.Kind == LedgerUsage {
						deductions++
					}
				}
				if deductions != reports {
					t.Errorf("usage deductions = %d, want %d", deductions, reports)
				}
			}
			exec := func(sql string) {
				t.Helper()
				if _, err := f.db.Pool.Exec(ctx(), sql); err != nil {
					t.Fatal(err)
				}
			}

			exec(`CREATE FUNCTION fail_usage() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN RAISE EXCEPTION 'usage fixture fault'; END;
			$$`)
			exec(tc.createFault)
			_, err := f.s.ReportUsage(ctx(), report)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Message != "usage fixture fault" {
				t.Fatalf("ReportUsage error = %v, want fixture fault", err)
			}
			checkState(30*usd, 0, 0)
			checkWake(false)
			if queued, _ := readEvents(); len(queued) != 0 {
				t.Errorf("failed report logged %d queued top-ups, want 0", len(queued))
			}
			exec(tc.dropFault)

			result, err := f.s.ReportUsage(ctx(), report)
			if err != nil {
				t.Fatal(err)
			}
			checkWake(true)
			wantDuplicate := result
			wantDuplicate.Duplicate = true
			duplicate, err := f.s.ReportUsage(ctx(), report)
			if err != nil || duplicate != wantDuplicate {
				t.Fatalf("duplicate report = %+v, %v, want %+v, nil", duplicate, err, wantDuplicate)
			}
			checkWake(false)
			checkState(5*usd, 1, 1)

			var topUpID string
			if err := f.db.Pool.QueryRow(ctx(), `SELECT top_up_id FROM top_ups
				WHERE billing_account_id = $1 AND kind = 'auto' AND status = 'pending'`, accountID).Scan(&topUpID); err != nil {
				t.Fatal(err)
			}
			queued, skipped := readEvents()
			if len(queued) != 1 || len(skipped) != 0 {
				t.Fatalf("retry and duplicate logged %d queued and %d skipped, want 1 and 0", len(queued), len(skipped))
			}
			e := queued[0]
			if e.TopUpID != topUpID || e.AccountID != accountID || e.Level != "INFO" ||
				e.Amount != 25*usd || e.Balance != 5*usd || e.MonthToDate == nil || *e.MonthToDate != 0 {
				t.Errorf("queued event = %+v, want committed top-up %s with queue-time amounts", e, topUpID)
			}

			report.WindowStart = report.WindowStart.Add(WindowLength)
			report.Amount = usd
			if _, err := f.s.ReportUsage(ctx(), report); err != nil {
				t.Fatal(err)
			}
			checkWake(false)
			checkState(4*usd, 2, 1)
			queued, skipped = readEvents()
			if len(queued) != 0 || len(skipped) != 1 || skipped[0].Reason != "in_flight" {
				t.Errorf("in-flight report logged queued = %+v, skipped = %+v", queued, skipped)
			}
		})
	}
}
