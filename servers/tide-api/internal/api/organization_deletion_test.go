// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
	"github.com/delinoio/mono/servers/tide-api/internal/app"
	"github.com/delinoio/mono/servers/tide-api/internal/testenv"
)

func TestOrganizationDeletionRechecksOwnerBeforeCleanup(t *testing.T) {
	var calls atomic.Int32
	env := testenv.New(t, testenv.WithExtension(func(r *app.Registry) error {
		r.OnOrganizationDeletion(api.OrganizationDeletionHook{
			Name: "cleanup",
			Prepare: func(_ context.Context, _ api.OrganizationDeletion) (api.OrganizationDeletionSteps, error) {
				calls.Add(1)
				return api.OrganizationDeletionSteps{}, nil
			},
		})
		return nil
	}))
	replica := env.NewReplica(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	org := env.NewOrg("alice")
	tx, err := replica.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := api.LockOrganization(ctx, tx, org.ID); err != nil {
		t.Fatal(err)
	}
	deleted := make(chan error, 1)
	go func() {
		_, err := env.User(org.Owner).Organization.DeleteOrganization(ctx, connect.NewRequest(&tidev1.DeleteOrganizationRequest{OrganizationId: org.ID}))
		deleted <- err
	}()
	waitForLockWaiter(t, env)
	// Commit the ownership change while the old owner's deletion waits
	// between its pre-check and its authoritative check.
	if _, err := tx.Exec(ctx, "UPDATE memberships SET role = $3 WHERE organization_id = $1 AND user_id = $2",
		org.ID, org.Owner, int16(tidev1.Role_ROLE_ADMIN)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "UPDATE memberships SET role = $3 WHERE organization_id = $1 AND user_id = $2",
		org.ID, org.Admin, int16(tidev1.Role_ROLE_OWNER)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wantCode(t, "former owner deletes", <-deleted, connect.CodePermissionDenied)
	if n := calls.Load(); n != 0 {
		t.Fatalf("cleanup calls after the owner check failed = %d, want 0", n)
	}
	resp, err := replica.User(org.Admin).Organization.GetOrganization(ctx, connect.NewRequest(&tidev1.GetOrganizationRequest{OrganizationId: org.ID}))
	if err != nil || resp.Msg.GetOrganization().GetCallerRole() != tidev1.Role_ROLE_OWNER {
		t.Fatalf("get organization as new owner: %v, %v", resp, err)
	}
	t.Log("Former owner's deletion returned permission_denied with zero cleanup calls. The new owner could still read the organization.")
}

func TestOrganizationDeletionSerializesWithoutOwnSandLink(t *testing.T) {
	var calls atomic.Int32
	preparing := make(chan struct{})
	allowFailure := make(chan struct{})
	ext := testenv.WithExtension(func(r *app.Registry) error {
		r.OnOrganizationDeletion(api.OrganizationDeletionHook{
			Name: "cleanup",
			Prepare: func(ctx context.Context, _ api.OrganizationDeletion) (api.OrganizationDeletionSteps, error) {
				if calls.Add(1) != 1 {
					return api.OrganizationDeletionSteps{}, nil
				}
				close(preparing)
				select {
				case <-ctx.Done():
					return api.OrganizationDeletionSteps{}, ctx.Err()
				case <-allowFailure:
					return api.OrganizationDeletionSteps{}, connect.NewError(connect.CodeUnavailable, errors.New("provider unavailable"))
				}
			},
		})
		return nil
	})
	env := testenv.New(t, ext)
	replica := env.NewReplica(t, ext)
	org := env.NewOrg("alice")
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	deleteOrg := func(e *testenv.Env, user string) error {
		_, err := e.User(user).Organization.DeleteOrganization(ctx, connect.NewRequest(&tidev1.DeleteOrganizationRequest{OrganizationId: org.ID}))
		return err
	}
	transfer := func() error {
		_, err := replica.User(org.Owner).Organization.TransferOwnership(ctx, connect.NewRequest(&tidev1.TransferOwnershipRequest{
			OrganizationId: org.ID, UserId: org.Admin,
		}))
		return err
	}
	deleted := make(chan error, 1)
	go func() { deleted <- deleteOrg(env, org.Owner) }()
	select {
	case <-preparing:
	case <-ctx.Done():
		t.Fatal("deletion did not reach cleanup")
	}
	wantCode(t, "concurrent deletion", deleteOrg(replica, org.Owner), connect.CodeFailedPrecondition)
	wantCode(t, "concurrent transfer", transfer(), connect.CodeFailedPrecondition)
	if n := calls.Load(); n != 1 {
		t.Fatalf("cleanup calls during active intent = %d, want 1", n)
	}

	close(allowFailure)
	wantCode(t, "failed preparation", <-deleted, connect.CodeUnavailable)
	if err := transfer(); err != nil {
		t.Fatalf("transfer after controlled failure: %v", err)
	}
	if err := deleteOrg(replica, org.Admin); err != nil {
		t.Fatalf("deletion by the new owner: %v", err)
	}
	t.Log("Competing deletion and transfer returned failed_precondition without an OwnSand link. After controlled failure, transfer and deletion by the new owner succeeded.")
}
