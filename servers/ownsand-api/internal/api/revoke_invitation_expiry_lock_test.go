// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

func TestRevokeInvitationExpiryAfterLockWait(t *testing.T) {
	for _, mode := range []pgx.QueryExecMode{pgx.QueryExecModeCacheDescribe, pgx.QueryExecModeSimpleProtocol, pgx.QueryExecModeCacheStatement} {
		t.Run(mode.String(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			expiresWhileWaiting := mode != pgx.QueryExecModeCacheStatement
			lockNotice := make(chan struct{}, 1)
			env := testenv.New(t, func(o *app.Options) {
				cfg := o.DB.Pool.Config()
				cfg.ConnConfig.DefaultQueryExecMode = mode
				if expiresWhileWaiting {
					var canSet bool
					if err := o.DB.Pool.QueryRow(ctx, "SELECT has_parameter_privilege(current_user, 'log_lock_waits', 'SET')").Scan(&canSet); err != nil {
						t.Fatal(err)
					}
					if !canSet {
						t.Skip("the test database role needs SET privilege on log_lock_waits")
					}
					cfg.ConnConfig.RuntimeParams["log_lock_waits"] = "on"
					cfg.ConnConfig.RuntimeParams["client_min_messages"] = "log"
					cfg.ConnConfig.OnNotice = func(_ *pgconn.PgConn, notice *pgconn.Notice) {
						if strings.Contains(notice.Message, "still waiting for") {
							select {
							case lockNotice <- struct{}{}:
							default:
							}
						}
					}
				}
				pool, err := pgxpool.NewWithConfig(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(pool.Close)
				o.DB = &db.DB{Pool: pool}
			})
			org := env.NewOrg("alice")
			invID, _ := newInvitation(t, org)

			holder, err := env.DB.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = holder.Rollback(context.Background()) }()
			var expires time.Time
			if err := holder.QueryRow(ctx, "SELECT expire_time FROM invitations WHERE invitation_id = $1 FOR UPDATE", invID).Scan(&expires); err != nil {
				t.Fatal(err)
			}

			type result struct {
				resp *connect.Response[ownsandv1.RevokeInvitationResponse]
				err  error
			}
			done := make(chan result, 1)
			go func() {
				resp, err := org.OwnerClient.Member.RevokeInvitation(ctx, connect.NewRequest(&ownsandv1.RevokeInvitationRequest{
					OrganizationId: org.ID, InvitationId: invID,
				}))
				done <- result{resp: resp, err: err}
			}()

			decisionTime := expires.Add(-time.Second)
			if expiresWhileWaiting {
				// The notice flushes RowDescription, which pgx reads before
				// QueryRow returns in these modes. The callback therefore
				// runs from Scan while the invitation row is still locked.
				select {
				case <-lockNotice:
				case result := <-done:
					t.Fatalf("revocation returned before the lock-wait notice: %v", result.err)
				case <-ctx.Done():
					t.Fatal("PostgreSQL did not send a lock-wait notice")
				}
				decisionTime = expires
			} else {
				waitLockWaiters(t, env, 1)
			}
			env.Clock.StepWall(decisionTime.Sub(env.Clock.Now()))
			if err := holder.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			got := <-done
			if expiresWhileWaiting {
				var rpcErr *connect.Error
				if !errors.As(got.err, &rpcErr) || rpcErr.Code() != connect.CodeFailedPrecondition || rpcErr.Message() != "only a pending invitation can be revoked" {
					t.Errorf("revocation at expiry returned %v, want failed_precondition for a non-pending invitation", got.err)
				}
			} else if got.err != nil {
				t.Errorf("revocation before expiry: %v", got.err)
			} else if state := got.resp.Msg.GetInvitation().GetState(); state != ownsandv1.InvitationState_INVITATION_STATE_REVOKED {
				t.Errorf("invitation state = %v, want REVOKED", state)
			}

			var revoked *time.Time
			if err := env.DB.Pool.QueryRow(ctx, "SELECT revoke_time FROM invitations WHERE invitation_id = $1", invID).Scan(&revoked); err != nil {
				t.Fatal(err)
			}
			var audits int
			if err := env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE organization_id = $1 AND target_id = $2 AND action = $3",
				org.ID, invID, int16(ownsandv1.AuditAction_AUDIT_ACTION_INVITATION_REVOKED)).Scan(&audits); err != nil {
				t.Fatal(err)
			}
			if expiresWhileWaiting {
				if revoked != nil || audits != 0 {
					t.Errorf("revoke_time = %v, revocation events = %d, want NULL and zero", revoked, audits)
				}
			} else if revoked == nil || !revoked.Equal(decisionTime) || audits != 1 {
				t.Errorf("revoke_time = %v, revocation events = %d, want %v and one", revoked, audits, decisionTime)
			}
		})
	}
}
