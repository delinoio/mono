// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

type statusLog struct {
	Message          string       `json:"msg"`
	BillingAccountID string       `json:"billing_account_id"`
	Service          string       `json:"service"`
	OrganizationID   string       `json:"organization_id"`
	From             LinkStatus   `json:"from"`
	To               LinkStatus   `json:"to"`
	Reason           StatusReason `json:"reason"`
	Sequence         int64        `json:"sequence"`
}

func (c *billingTelemetryCapture) wantStatusObservations(t *testing.T, wantCounts map[LinkStatus]int64, wantLogs ...statusLog) {
	t.Helper()
	var logs []statusLog
	dec := json.NewDecoder(bytes.NewReader(c.logs.Bytes()))
	for dec.More() {
		var entry statusLog
		if err := dec.Decode(&entry); err != nil {
			t.Fatal(err)
		}
		switch entry.Message {
		case "billing_organization_blocked", "billing_organization_suspended", "billing_organization_resumed", "billing_status_changed":
			logs = append(logs, entry)
		}
	}
	if !slices.Equal(logs, wantLogs) {
		t.Errorf("status logs = %+v, want %+v", logs, wantLogs)
	}

	var data metricdata.ResourceMetrics
	if err := c.reader.Collect(ctx(), &data); err != nil {
		t.Fatal(err)
	}
	counts := map[LinkStatus]int64{}
	for _, scope := range data.ScopeMetrics {
		for _, instrument := range scope.Metrics {
			if instrument.Name != "accounts_billing_status_changes" {
				continue
			}
			sum, ok := instrument.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("status counter data is %T, want Sum[int64]", instrument.Data)
			}
			for _, point := range sum.DataPoints {
				status, _ := point.Attributes.Value("status")
				counts[LinkStatus(status.AsString())] += point.Value
			}
		}
	}
	if !maps.Equal(counts, wantCounts) {
		t.Errorf("status counters = %v, want %v", counts, wantCounts)
	}
}

func TestStatusObservationsRequireCommit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		trigger string
	}{
		{"notification_insert", "CREATE TRIGGER reject_blocked_notice BEFORE INSERT ON billing_notifications"},
		{"deferred_constraint", "CREATE CONSTRAINT TRIGGER reject_blocked_notice AFTER INSERT ON billing_notifications DEFERRABLE INITIALLY DEFERRED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			acct := f.account()
			f.activate(acct)
			f.credit(acct, 1000)
			orgs := []string{f.link(acct, "First"), f.link(acct, "Second")}
			slices.Sort(orgs)
			initial := []OrganizationStatus{
				f.wantStatus(orgs[0], StatusAllowed, ReasonNone),
				f.wantStatus(orgs[1], StatusAllowed, ReasonNone),
			}
			lastSequence := max(initial[0].Sequence, initial[1].Sequence)
			telemetry := f.captureBillingTelemetry()
			telemetry.wantStatusObservations(t, nil)

			if _, err := f.db.Pool.Exec(ctx(), fmt.Sprintf(`
				CREATE FUNCTION reject_blocked_notice() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN
					RAISE EXCEPTION 'blocked notice rejected' USING ERRCODE = '23514', CONSTRAINT = 'reject_blocked_notice';
				END
				$$;
				%s FOR EACH ROW WHEN (NEW.kind = 'organization_blocked' AND NEW.details->>'organization_id' = '%s')
				EXECUTE FUNCTION reject_blocked_notice()`, tc.trigger, orgs[1])); err != nil {
				t.Fatal(err)
			}
			_, err := f.deductWithError(acct, 10*usd)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.ConstraintName != "reject_blocked_notice" {
				t.Fatalf("deduction error = %v, want rejected blocked notice", err)
			}
			for i, org := range orgs {
				got := f.wantStatus(org, StatusAllowed, ReasonNone)
				if got.Sequence != initial[i].Sequence || got.LastAllowedSequence != initial[i].LastAllowedSequence {
					t.Fatalf("link %s sequence changed on rollback: %+v", org, got)
				}
			}
			if got := f.balance(acct); got != 10*usd {
				t.Errorf("balance after rollback = %d, want %d", got, 10*usd)
			}
			if got := f.count(`SELECT count(*) FROM billing_notifications WHERE kind = 'organization_blocked'`); got != 0 {
				t.Errorf("blocked notifications after rollback = %d, want 0", got)
			}
			telemetry.wantStatusObservations(t, nil)

			if _, err := f.db.Pool.Exec(ctx(), "DROP TRIGGER reject_blocked_notice ON billing_notifications"); err != nil {
				t.Fatal(err)
			}
			f.deduct(acct, 10*usd)
			if got := f.balance(acct); got != 0 {
				t.Errorf("balance after retry = %d, want 0", got)
			}
			var wantLogs []statusLog
			for i, org := range orgs {
				got := f.wantStatus(org, StatusBlocked, ReasonNonPositiveBalance)
				wantSequence := lastSequence + int64(i) + 1
				if got.Sequence != wantSequence || got.LastAllowedSequence != initial[i].LastAllowedSequence {
					t.Fatalf("link %s after retry = %+v, want sequence %d and last allowed sequence %d",
						org, got, wantSequence, initial[i].LastAllowedSequence)
				}
				if got := f.count(`SELECT count(*) FROM billing_notifications
					WHERE kind = 'organization_blocked' AND details->>'organization_id' = $1`, org); got != 1 {
					t.Errorf("blocked notifications for %s = %d, want 1", org, got)
				}
				wantLogs = append(wantLogs, statusLog{"billing_organization_blocked", acct, "ownsand", org,
					StatusAllowed, StatusBlocked, ReasonNonPositiveBalance, wantSequence})
			}
			telemetry.wantStatusObservations(t, map[LinkStatus]int64{StatusBlocked: 2}, wantLogs...)
			if err := f.s.SyncAccount(ctx(), acct); err != nil {
				t.Fatal(err)
			}
			telemetry.wantStatusObservations(t, map[LinkStatus]int64{StatusBlocked: 2}, wantLogs...)
		})
	}
}

func TestStatusObservationsForReasonAndTimestampChanges(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	acct := f.account()
	org := f.link(acct, "Sandbox")
	initial := f.wantStatus(org, StatusBlocked, ReasonNoActiveSubscription)
	telemetry := f.captureBillingTelemetry()

	f.clock.Advance(time.Hour)
	f.activate(acct)
	if err := f.s.SyncAccount(ctx(), acct); err != nil {
		t.Fatal(err)
	}
	reasonChange := f.wantStatus(org, StatusBlocked, ReasonNonPositiveBalance)
	if !sameTime(initial.BlockedSince, reasonChange.BlockedSince) || !sameTime(initial.SuspendTime, reasonChange.SuspendTime) ||
		reasonChange.Sequence != initial.Sequence+1 {
		t.Fatalf("reason-only change = %+v, initial = %+v", reasonChange, initial)
	}
	want := statusLog{"billing_status_changed", acct, "ownsand", org, StatusBlocked, StatusBlocked,
		ReasonNonPositiveBalance, reasonChange.Sequence}
	telemetry.wantStatusObservations(t, nil, want)

	// Recompute a stale suspension timestamp without changing the reason or phase.
	if _, err := f.db.Pool.Exec(ctx(), "UPDATE organization_links SET suspend_time = suspend_time + interval '1 hour'"); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SyncAccount(ctx(), acct); err != nil {
		t.Fatal(err)
	}
	got := f.wantStatus(org, StatusBlocked, ReasonNonPositiveBalance)
	if !sameTime(initial.BlockedSince, got.BlockedSince) || !sameTime(initial.SuspendTime, got.SuspendTime) ||
		got.Sequence != reasonChange.Sequence+1 {
		t.Fatalf("timestamp-only change = %+v, before = %+v", got, reasonChange)
	}
	wantTimestamp := want
	wantTimestamp.Sequence++
	telemetry.wantStatusObservations(t, nil, want, wantTimestamp)
	if got := f.count(`SELECT count(*) FROM billing_notifications WHERE kind = 'organization_blocked'`); got != 1 {
		t.Errorf("blocked notifications = %d, want 1 from the initial block", got)
	}
}
