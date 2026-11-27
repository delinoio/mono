// SPDX-License-Identifier: FSL-1.1-ALv2

package billing_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/billing"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db/dbtest"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

func TestApplyRemoteAfterBlockStartMigration(t *testing.T) {
	localStart := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	oldDeadline := localStart.Add(72 * time.Hour)
	firstRemote := localStart.Add(time.Hour)
	newBlock := localStart.Add(48 * time.Hour)
	now := oldDeadline.Add(time.Minute)

	const cachedSequence, recoverySequence, newSequence int64 = 5, 7, 8
	for _, tc := range []struct {
		name                string
		phase               accountsv1.BillingStatus
		account             string
		sequence, recovery  int64
		remoteStart         time.Time
		wantStart           time.Time
		wantSequence        int64
		wantPhase           ownsandv1.BillingPhase
		wantRemote, applied bool
	}{
		{"blocked replay", remoteBlocked, accountA, cachedSequence, 0, firstRemote, localStart, cachedSequence, billing.PhaseBlocked, true, false},
		{"suspended replay", remoteSuspended, accountA, cachedSequence, 0, firstRemote, localStart, cachedSequence, billing.PhaseBlocked, true, false},
		{"missed recovery", remoteBlocked, accountA, newSequence, recoverySequence, newBlock, newBlock, newSequence, billing.PhaseBlocked, true, true},
		{"old producer", remoteBlocked, accountA, newSequence, 0, newBlock, localStart, newSequence, billing.PhaseSuspended, true, true},
		{"continuing nonpayment", remoteSuspended, accountA, newSequence, cachedSequence - 1, firstRemote, localStart, newSequence, billing.PhaseSuspended, true, true},
		{"invalid recovery range", remoteBlocked, accountA, newSequence, newSequence, newBlock, localStart, newSequence, billing.PhaseSuspended, true, true},
		{"missing timestamp", remoteBlocked, accountA, newSequence, recoverySequence, time.Time{}, localStart, newSequence, billing.PhaseSuspended, false, true},
		{"older sequence", remoteBlocked, accountA, cachedSequence - 1, 0, firstRemote, localStart, cachedSequence, billing.PhaseBlocked, false, false},
		{"another account", remoteBlocked, accountB, newSequence, recoverySequence, newBlock, localStart, cachedSequence, billing.PhaseBlocked, false, false},
		{"another account replay", remoteBlocked, accountB, cachedSequence, 0, firstRemote, localStart, cachedSequence, billing.PhaseBlocked, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			d, err := db.Open(ctx, dbtest.NewURLAtVersion(t, 15))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(d.Close)
			if _, err := d.Pool.Exec(ctx, "INSERT INTO organizations (organization_id, name, create_time) VALUES ($1, 'org', $2)", org1, localStart); err != nil {
				t.Fatal(err)
			}
			if _, err := d.Pool.Exec(ctx, `UPDATE organization_billing SET billing_account_id = $2, phase = 2, reason = 2,
				blocked_since = $3, suspend_time = $4, status_sync_time = $3, status_sequence = $5, update_time = $3
				WHERE organization_id = $1`, org1, accountA, localStart, oldDeadline, cachedSequence); err != nil {
				t.Fatal(err)
			}
			if err := d.Migrate(ctx, testenv.Discard()); err != nil {
				t.Fatal(err)
			}
			r := &accountsv1.OrganizationBillingStatus{
				Service: billing.ServiceID, OrganizationId: org1, BillingAccountId: tc.account,
				Status: tc.phase, Sequence: tc.sequence, LastAllowedSequence: tc.recovery,
			}
			if !tc.remoteStart.IsZero() {
				r.BlockedSince = timestamppb.New(tc.remoteStart)
			}
			apply := func(at time.Time, wantApplied bool) billing.Status {
				t.Helper()
				if err := d.InTx(ctx, func(tx pgx.Tx) error {
					_, applied, err := billing.ApplyRemoteTx(ctx, tx, r, billing.SourcePoll, at)
					if err == nil && applied != wantApplied {
						t.Errorf("applied = %v, want %v", applied, wantApplied)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				st, ok, err := billing.LoadStatus(ctx, d.Pool, org1, false)
				if err != nil || !ok {
					t.Fatalf("load status: %v, %v", ok, err)
				}
				return st
			}
			check := func(st billing.Status) {
				t.Helper()
				if st.Sequence != tc.wantSequence || st.Phase != tc.wantPhase || !equalTime(st.BlockedSince, tc.wantStart) ||
					!st.SuspendTime().Equal(tc.wantStart.Add(72*time.Hour)) ||
					(tc.wantRemote && !equalTime(st.RemoteBlockedSince, tc.remoteStart)) ||
					(!tc.wantRemote && st.RemoteBlockedSince != nil) {
					t.Fatalf("cached status = %+v, deadline %v", st, st.SuspendTime())
				}
			}
			st := apply(now, tc.applied)
			check(st)
			if tc.account == accountA && tc.sequence >= cachedSequence && !equalTime(st.SyncTime, now) {
				t.Fatalf("sync time = %v, want %v", st.SyncTime, now)
			}
			if tc.wantStart.Equal(newBlock) {
				newDeadline := newBlock.Add(72 * time.Hour)
				if st.Confirmed(oldDeadline) != billing.PhaseBlocked || st.Confirmed(newDeadline.Add(-time.Microsecond)) != billing.PhaseBlocked ||
					st.Confirmed(newDeadline) != billing.PhaseSuspended {
					t.Fatalf("new interval phases at old and new deadlines: %s / %s", st.Confirmed(oldDeadline), st.Confirmed(newDeadline))
				}
				check(apply(now.Add(time.Minute), false))
			}
		})
	}
}
