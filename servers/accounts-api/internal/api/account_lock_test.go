// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/servers/accounts-api/internal/testenv"
)

// businessState is the state a rejected mutation must leave unchanged.
type businessState struct {
	Roles       string
	Invitations int
	AuditEvents int
	Deleted     bool
}

func readBusinessState(t *testing.T, env *testenv.Env, accountID string) businessState {
	t.Helper()
	var s businessState
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT COALESCE(string_agg(user_id || '=' || role, ',' ORDER BY user_id), '')
		FROM billing_account_members WHERE billing_account_id = $1`, accountID).Scan(&s.Roles); err != nil {
		t.Fatal(err)
	}
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT count(*) FROM billing_account_invitations WHERE billing_account_id = $1`, accountID).Scan(&s.Invitations); err != nil {
		t.Fatal(err)
	}
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT count(*) FROM billing_audit_events WHERE billing_account_id = $1`, accountID).Scan(&s.AuditEvents); err != nil {
		t.Fatal(err)
	}
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT delete_time IS NOT NULL FROM billing_accounts WHERE billing_account_id = $1`, accountID).Scan(&s.Deleted); err != nil {
		t.Fatal(err)
	}
	return s
}

// raceMembershipChange holds the billing account lock in a second
// connection, starts call, waits until call blocks on the lock, applies
// change in the holder transaction, commits it and returns the error of
// call. It exercises a caller that was authorized before a serialized
// membership change and acquires the account lock after it.
func raceMembershipChange(t *testing.T, env *testenv.Env, accountID string, change func(tx pgx.Tx), call func() error) error {
	t.Helper()
	return raceLockWait(t, env, "billing account lock", func(tx pgx.Tx) {
		var locked int
		if err := tx.QueryRow(ctx(), `SELECT 1 FROM billing_accounts WHERE billing_account_id = $1 FOR UPDATE`, accountID).Scan(&locked); err != nil {
			t.Fatal(err)
		}
	}, change, call)
}

// raceLockWait runs an RPC while a separate transaction holds a lock.
// It runs change only after Postgres confirms the RPC is waiting on the holder.
func raceLockWait(t *testing.T, env *testenv.Env, lockName string, lock, change func(pgx.Tx), call func() error) error {
	t.Helper()
	tx, err := env.DB.Pool.Begin(ctx())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx()) }()
	var holderPID int
	if err := tx.QueryRow(ctx(), `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatal(err)
	}
	lock(tx)

	done := make(chan error, 1)
	go func() { done <- call() }()

	// Confirm that the RPC waits for the holder's lock.
	deadline := time.Now().Add(15 * time.Second)
	for {
		var waiting int
		if err := env.DB.Pool.QueryRow(ctx(), `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'
			AND $1 = ANY(pg_blocking_pids(pid))`, holderPID).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("the RPC returned (%v) before it waited for the %s", err, lockName)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the RPC did not wait for the %s", lockName)
		}
		time.Sleep(10 * time.Millisecond)
	}

	change(tx)
	if err := tx.Commit(ctx()); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		return err
	case <-time.After(15 * time.Second):
		t.Fatal("the RPC did not finish after the lock holder committed")
		return nil
	}
}

func execTx(t *testing.T, tx pgx.Tx, sql string, args ...any) {
	t.Helper()
	if _, err := tx.Exec(ctx(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

// TestMutationUsesMembershipAfterLockWait covers a caller that waits for the
// account lock while another transaction changes the caller's membership:
// the mutation must see the committed membership, not the one from before
// the wait.
func TestMutationUsesMembershipAfterLockWait(t *testing.T) {
	demote := func(member func(f fixture) person) func(f fixture) func(tx pgx.Tx) {
		return func(f fixture) func(tx pgx.Tx) {
			return func(tx pgx.Tx) {
				execTx(t, tx, `UPDATE billing_account_members SET role = 'viewer' WHERE billing_account_id = $1 AND user_id = $2`, f.AccountID, member(f).ID)
			}
		}
	}
	remove := func(member func(f fixture) person) func(f fixture) func(tx pgx.Tx) {
		return func(f fixture) func(tx pgx.Tx) {
			return func(tx pgx.Tx) {
				execTx(t, tx, `DELETE FROM billing_account_members WHERE billing_account_id = $1 AND user_id = $2`, f.AccountID, member(f).ID)
			}
		}
	}
	// A committed TransferOwnership: the viewer becomes the owner and the
	// previous owner an admin.
	transfer := func(f fixture) func(tx pgx.Tx) {
		return func(tx pgx.Tx) {
			execTx(t, tx, `UPDATE billing_account_members SET role = 'admin' WHERE billing_account_id = $1 AND user_id = $2`, f.AccountID, f.Owner.ID)
			execTx(t, tx, `UPDATE billing_account_members SET role = 'owner' WHERE billing_account_id = $1 AND user_id = $2`, f.AccountID, f.Viewer.ID)
		}
	}
	createInvitation := func(p person, f fixture) func() error {
		return func() error {
			_, err := p.Client.CreateInvitation(ctx(), connect.NewRequest(&accountsv1.CreateInvitationRequest{
				BillingAccountId: f.AccountID, Email: fmt.Sprintf("late-%d@example.com", userSeq.Add(1)), Role: roleViewer,
			}))
			return err
		}
	}
	deleteAccount := func(p person, f fixture) func() error {
		return func() error {
			_, err := p.Client.DeleteBillingAccount(ctx(), connect.NewRequest(&accountsv1.DeleteBillingAccountRequest{BillingAccountId: f.AccountID}))
			return err
		}
	}
	transferOwnership := func(p person, f fixture) func() error {
		return func() error {
			_, err := p.Client.TransferOwnership(ctx(), connect.NewRequest(&accountsv1.TransferOwnershipRequest{BillingAccountId: f.AccountID, UserId: f.Viewer2.ID}))
			return err
		}
	}

	tests := []struct {
		name   string
		change func(f fixture) func(tx pgx.Tx)
		caller func(f fixture) person
		call   func(p person, f fixture) func() error
		want   connect.Code
	}{
		{"demoted admin cannot create an invitation", demote(func(f fixture) person { return f.Admin }), func(f fixture) person { return f.Admin }, createInvitation, connect.CodePermissionDenied},
		{"removed admin cannot create an invitation", remove(func(f fixture) person { return f.Admin }), func(f fixture) person { return f.Admin }, createInvitation, connect.CodeNotFound},
		{"former owner cannot delete the account after a transfer", transfer, func(f fixture) person { return f.Owner }, deleteAccount, connect.CodePermissionDenied},
		{"former owner cannot transfer ownership after a transfer", transfer, func(f fixture) person { return f.Owner }, transferOwnership, connect.CodePermissionDenied},
		{"removed owner cannot delete the account", remove(func(f fixture) person { return f.Owner }), func(f fixture) person { return f.Owner }, deleteAccount, connect.CodeNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.New(t)
			f := newFixture(t, env)
			before := readBusinessState(t, env, f.AccountID)
			err := raceMembershipChange(t, env, f.AccountID, tc.change(f), tc.call(tc.caller(f), f))
			wantCode(t, err, tc.want)

			// The holder's membership change is the only business change; the
			// rejected RPC adds no invitation, no success audit event and no
			// deletion.
			after := readBusinessState(t, env, f.AccountID)
			if after.Roles == before.Roles {
				t.Fatalf("the holder did not change the memberships: %q", after.Roles)
			}
			if after.Invitations != before.Invitations || after.AuditEvents != before.AuditEvents || after.Deleted {
				t.Fatalf("a rejected operation changed the business state: before=%+v after=%+v", before, after)
			}
		})
	}
}

// TestMutationAfterUnrelatedLockWaitSucceeds is the control of
// TestMutationUsesMembershipAfterLockWait: a caller whose membership does not
// change while it waits for the account lock still succeeds.
func TestMutationAfterUnrelatedLockWaitSucceeds(t *testing.T) {
	env := testenv.New(t)
	f := newFixture(t, env)
	before := readBusinessState(t, env, f.AccountID)
	err := raceMembershipChange(t, env, f.AccountID, func(tx pgx.Tx) {
		execTx(t, tx, `UPDATE billing_account_members SET role = 'viewer' WHERE billing_account_id = $1 AND user_id = $2`, f.AccountID, f.Viewer2.ID)
	}, func() error {
		_, err := f.Admin.Client.CreateInvitation(ctx(), connect.NewRequest(&accountsv1.CreateInvitationRequest{
			BillingAccountId: f.AccountID, Email: fmt.Sprintf("control-%d@example.com", userSeq.Add(1)), Role: roleViewer,
		}))
		return err
	})
	wantCode(t, err, 0)
	after := readBusinessState(t, env, f.AccountID)
	if after.Invitations != before.Invitations+1 || after.AuditEvents != before.AuditEvents+1 {
		t.Fatalf("the invitation was not recorded: before=%+v after=%+v", before, after)
	}
}
