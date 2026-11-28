// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

func TestCreateInvitationDuringMembershipChange(t *testing.T) {
	t.Parallel()
	for _, change := range []struct {
		name       string
		write      string
		rejectCode connect.Code
		message    string
	}{
		{"removal", "DELETE FROM memberships WHERE organization_id = $1 AND user_id = $2", connect.CodeNotFound, "organization not found"},
		{"demotion", "UPDATE memberships SET role = 3 WHERE organization_id = $1 AND user_id = $2", connect.CodePermissionDenied, "the caller's role does not allow this action"},
	} {
		t.Run(change.name+"/change_first", func(t *testing.T) {
			env := testenv.New(t)
			org := env.NewOrg("alice")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			before := orgRecordCounts(t, env, org.ID, "invitations")
			holder := beginInvitationLockFixture(t, env, ctx)
			if _, err := holder.Exec(ctx, "SELECT 1 FROM memberships WHERE organization_id = $1 AND user_id = $2 FOR UPDATE", org.ID, org.Admin); err != nil {
				t.Fatal(err)
			}
			if _, err := holder.Exec(ctx, change.write, org.ID, org.Admin); err != nil {
				t.Fatal(err)
			}

			admin := env.User(org.Admin)
			result := make(chan error, 1)
			go func() {
				_, err := admin.Member.CreateInvitation(ctx, connect.NewRequest(&ownsandv1.CreateInvitationRequest{
					OrganizationId: org.ID, Role: ownsandv1.Role_ROLE_MEMBER,
				}))
				result <- err
			}()
			awaitInvitationLock(t, ctx, env.DB.Pool, holder.Conn().PgConn().PID(), "SELECT role FROM memberships%FOR SHARE")
			if err := holder.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			var rpcErr *connect.Error
			if err := <-result; !errors.As(err, &rpcErr) || rpcErr.Code() != change.rejectCode || rpcErr.Message() != change.message {
				t.Fatalf("create invitation = %v, want %s: %s", err, change.rejectCode, change.message)
			}
			if after := orgRecordCounts(t, env, org.ID, "invitations"); after != before {
				t.Fatalf("invitation, audit and control event counts changed from %v to %v", before, after)
			}
		})

		t.Run(change.name+"/create_first", func(t *testing.T) {
			env := testenv.New(t)
			org := env.NewOrg("alice")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			holder := beginInvitationLockFixture(t, env, ctx)
			// Pause the real CreateInvitation insert after it has taken both
			// row locks. The membership RPC must then wait for creation.
			if _, err := holder.Exec(ctx, "LOCK TABLE invitations IN SHARE MODE"); err != nil {
				t.Fatal(err)
			}
			type createResult struct {
				response *connect.Response[ownsandv1.CreateInvitationResponse]
				err      error
			}
			admin := env.User(org.Admin)
			created := make(chan createResult, 1)
			go func() {
				response, err := admin.Member.CreateInvitation(ctx, connect.NewRequest(&ownsandv1.CreateInvitationRequest{
					OrganizationId: org.ID, Role: ownsandv1.Role_ROLE_ADMIN,
				}))
				created <- createResult{response, err}
			}()
			creatorPID := awaitInvitationLock(t, ctx, env.DB.Pool, holder.Conn().PgConn().PID(), "INSERT INTO invitations%")
			changed := make(chan error, 1)
			go func() {
				if change.name == "removal" {
					_, err := org.OwnerClient.Member.RemoveMember(ctx, connect.NewRequest(&ownsandv1.RemoveMemberRequest{
						OrganizationId: org.ID, UserId: org.Admin,
					}))
					changed <- err
				} else {
					_, err := org.OwnerClient.Member.UpdateMemberRole(ctx, connect.NewRequest(&ownsandv1.UpdateMemberRoleRequest{
						OrganizationId: org.ID, UserId: org.Admin, Role: ownsandv1.Role_ROLE_MEMBER,
					}))
					changed <- err
				}
			}()
			awaitInvitationLock(t, ctx, env.DB.Pool, creatorPID, "SELECT membership_id::text, role FROM memberships%FOR UPDATE")
			if err := holder.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			creation := <-created
			if creation.err != nil {
				t.Fatal(creation.err)
			}
			if err := <-changed; err != nil {
				t.Fatal(err)
			}
			id := creation.response.Msg.GetInvitation().GetInvitationId()
			if got := invitationSettlement(t, env, id); got[0] != "" || got[1] == "" {
				t.Fatalf("concurrently created invitation %s settlement = %v, want revoked", id, got)
			}
			if got := countRows(t, env, "SELECT count(*) FROM audit_events WHERE organization_id = $1 AND action = $2 AND target_id = $3",
				org.ID, int16(ownsandv1.AuditAction_AUDIT_ACTION_INVITATION_REVOKED), id); got != 1 {
				t.Fatalf("revocation audits for %s = %d, want 1", id, got)
			}
		})
	}
}

func beginInvitationLockFixture(t *testing.T, env *testenv.Env, ctx context.Context) pgx.Tx {
	t.Helper()
	holder, err := env.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = holder.Rollback(ctx)
	})
	return holder
}

// Match the query, database and blocker so this fixture ignores unrelated
// transactions elsewhere on the PostgreSQL server.
func awaitInvitationLock(t *testing.T, ctx context.Context, pool queryRower, blockerPID uint32, queryPattern string) uint32 {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var pid uint32
		err := pool.QueryRow(ctx, `SELECT pid FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'
				AND $1 = ANY(pg_blocking_pids(pid)) AND query LIKE $2`, blockerPID, queryPattern).Scan(&pid)
		if err == nil {
			return pid
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("look for %q waiting on PID %d: %v", queryPattern, blockerPID, err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("query %q never waited on PID %d: %v", queryPattern, blockerPID, ctx.Err())
		case <-ticker.C:
		}
	}
}
