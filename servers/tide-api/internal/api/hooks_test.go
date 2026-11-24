// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
	"github.com/delinoio/mono/servers/tide-api/internal/app"
	"github.com/delinoio/mono/servers/tide-api/internal/lease"
	"github.com/delinoio/mono/servers/tide-api/internal/testenv"
)

type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	r.events = append(r.events, s)
	r.mu.Unlock()
}

func (r *recorder) get() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

func TestMembershipEndHooks(t *testing.T) {
	rec := &recorder{}
	var got api.MembershipEnd
	env := testenv.New(t, testenv.WithExtension(func(r *app.Registry) error {
		r.OnMembershipEnd(api.MembershipEndHook{
			Name: "test",
			InTx: func(ctx context.Context, tx pgx.Tx, e api.MembershipEnd) error {
				// The hook sees the revocation in its transaction.
				var active int
				if err := tx.QueryRow(ctx, "SELECT count(*) FROM api_keys WHERE organization_id = $1 AND user_id = $2 AND revoke_time IS NULL", e.OrganizationID, e.UserID).Scan(&active); err != nil {
					return err
				}
				if active != 0 {
					return errors.New("keys still active")
				}
				rec.add("in_tx")
				return nil
			},
			AfterCommit: func(ctx context.Context, e api.MembershipEnd) {
				got = e
				rec.add("after_commit")
			},
		})
		return nil
	}))
	ctx := context.Background()
	org := env.NewOrg("alice")
	k1, _ := env.NewAPIKey(org.ID, org.Member)
	k2, _ := env.NewAPIKey(org.ID, org.Member)
	env.NewAPIKey(org.ID, org.Owner)
	if _, err := env.User(org.Admin).Member.RemoveMember(ctx, connect.NewRequest(&tidev1.RemoveMemberRequest{OrganizationId: org.ID, UserId: org.Member})); err != nil {
		t.Fatal(err)
	}
	if e := rec.get(); !slices.Equal(e, []string{"in_tx", "after_commit"}) {
		t.Fatalf("hook calls %v", e)
	}
	slices.Sort(got.RevokedAPIKeyIDs)
	want := []string{k1, k2}
	slices.Sort(want)
	if got.UserID != org.Member || got.Reason != api.MembershipRemoved || got.Actor.UserID != org.Admin || !slices.Equal(got.RevokedAPIKeyIDs, want) {
		t.Fatalf("membership end %+v", got)
	}
	// The owner's key stays active.
	var active int
	_ = env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM api_keys WHERE organization_id = $1 AND revoke_time IS NULL", org.ID).Scan(&active)
	if active != 1 {
		t.Fatalf("%d active keys", active)
	}
	actions := auditActions(t, env, org.ID)
	if countAction(actions, tidev1.AuditAction_AUDIT_ACTION_API_KEY_REVOKED) != 2 || countAction(actions, tidev1.AuditAction_AUDIT_ACTION_MEMBER_REMOVED) != 1 {
		t.Fatalf("audit %v", actions)
	}
	// Leaving runs the hooks with MembershipLeft.
	if _, err := env.User(org.Admin).Member.RemoveMember(ctx, connect.NewRequest(&tidev1.RemoveMemberRequest{OrganizationId: org.ID, UserId: org.Admin})); err != nil {
		t.Fatal(err)
	}
	if got.Reason != api.MembershipLeft || got.Actor.UserID != org.Admin {
		t.Fatalf("leave %+v", got)
	}
}

func TestMembershipEndHookErrorRollsBack(t *testing.T) {
	env := testenv.New(t, testenv.WithExtension(func(r *app.Registry) error {
		r.OnMembershipEnd(api.MembershipEndHook{Name: "fails", InTx: func(context.Context, pgx.Tx, api.MembershipEnd) error {
			return errors.New("boom")
		}})
		return nil
	}))
	ctx := context.Background()
	org := env.NewOrg("alice")
	env.NewAPIKey(org.ID, org.Member)
	_, err := env.User(org.Owner).Member.RemoveMember(ctx, connect.NewRequest(&tidev1.RemoveMemberRequest{OrganizationId: org.ID, UserId: org.Member}))
	wantCode(t, "failing hook", err, connect.CodeInternal)
	var members, active int
	_ = env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM memberships WHERE organization_id = $1", org.ID).Scan(&members)
	_ = env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM api_keys WHERE organization_id = $1 AND revoke_time IS NULL", org.ID).Scan(&active)
	if members != 3 || active != 1 {
		t.Fatalf("members %d, active keys %d after a rollback", members, active)
	}
}

func TestDeleteOrganizationHooks(t *testing.T) {
	rec := &recorder{}
	failPrepare := true
	env := testenv.New(t, testenv.WithExtension(func(r *app.Registry) error {
		for _, name := range []string{"requests", "credentials"} {
			r.OnOrganizationDeletion(api.OrganizationDeletionHook{
				Name: name,
				Prepare: func(ctx context.Context, d api.OrganizationDeletion) (api.OrganizationDeletionSteps, error) {
					if failPrepare && name == "credentials" {
						return api.OrganizationDeletionSteps{}, connect.NewError(connect.CodeUnavailable, errors.New("ownsand down"))
					}
					rec.add("prepare:" + name)
					return api.OrganizationDeletionSteps{
						InTx: func(ctx context.Context, tx pgx.Tx) error {
							var keys int
							if err := tx.QueryRow(ctx, "SELECT count(*) FROM api_keys WHERE organization_id = $1 AND revoke_time IS NULL", d.OrganizationID).Scan(&keys); err != nil {
								return err
							}
							if keys == 0 {
								return errors.New("keys revoked before the hook")
							}
							rec.add("in_tx:" + name)
							return nil
						},
						AfterCommit: func(context.Context) { rec.add("after:" + name) },
					}, nil
				},
			})
		}
		return nil
	}))
	ctx := context.Background()
	org := env.NewOrg("alice")
	env.NewAPIKey(org.ID, org.Member)
	owner := env.User(org.Owner)
	_, err := env.User(org.Admin).Organization.DeleteOrganization(ctx, connect.NewRequest(&tidev1.DeleteOrganizationRequest{OrganizationId: org.ID}))
	wantCode(t, "admin deletes", err, connect.CodePermissionDenied)
	if len(rec.get()) != 0 {
		t.Fatalf("hooks ran for a rejected caller: %v", rec.get())
	}
	_, err = owner.Organization.DeleteOrganization(ctx, connect.NewRequest(&tidev1.DeleteOrganizationRequest{OrganizationId: org.ID}))
	wantCode(t, "failing prepare", err, connect.CodeUnavailable)
	if _, err := owner.Organization.GetOrganization(ctx, connect.NewRequest(&tidev1.GetOrganizationRequest{OrganizationId: org.ID})); err != nil {
		t.Fatalf("organization gone after a failed prepare: %v", err)
	}
	failPrepare = false
	rec.mu.Lock()
	rec.events = nil
	rec.mu.Unlock()
	if _, err := owner.Organization.DeleteOrganization(ctx, connect.NewRequest(&tidev1.DeleteOrganizationRequest{OrganizationId: org.ID})); err != nil {
		t.Fatal(err)
	}
	want := []string{"prepare:requests", "prepare:credentials", "in_tx:requests", "in_tx:credentials", "after:requests", "after:credentials"}
	if got := rec.get(); !slices.Equal(got, want) {
		t.Fatalf("hook order %v, want %v", got, want)
	}
	_, err = owner.Organization.GetOrganization(ctx, connect.NewRequest(&tidev1.GetOrganizationRequest{OrganizationId: org.ID}))
	wantCode(t, "after deletion", err, connect.CodeNotFound)
	for _, table := range []string{"memberships", "invitations", "api_keys"} {
		var n int
		_ = env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE organization_id = $1", org.ID).Scan(&n)
		if n != 0 {
			t.Errorf("%d rows left in %s", n, table)
		}
	}
	// The audit events stay and include the key revocation and the deletion.
	actions := auditActions(t, env, org.ID)
	if actions[0] != tidev1.AuditAction_AUDIT_ACTION_ORGANIZATION_DELETED || countAction(actions, tidev1.AuditAction_AUDIT_ACTION_API_KEY_REVOKED) != 1 {
		t.Fatalf("audit %v", actions)
	}
}

// TestLeaseComponents checks that a component that a layer registers runs
// on the lease holder.
func TestLeaseComponents(t *testing.T) {
	started := make(chan struct{}, 1)
	env := testenv.New(t, testenv.WithExtension(func(r *app.Registry) error {
		r.AddLeaseComponent(lease.ComponentFunc{ComponentName: "test", RunFunc: func(ctx context.Context) error {
			started <- struct{}{}
			<-ctx.Done()
			return nil
		}})
		return nil
	}))
	select {
	case <-started:
	case <-time.After(15 * time.Second):
		t.Fatal("lease component did not start")
	}
	if !env.App.Lease.Held() {
		t.Fatal("lease not held")
	}
}
