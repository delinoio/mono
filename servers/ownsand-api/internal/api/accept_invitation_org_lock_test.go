// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// newInvitation creates a pending invitation of the organization and returns
// its ID and secret.
func newInvitation(t *testing.T, org *testenv.Org) (id, secret string) {
	t.Helper()
	inv, err := org.OwnerClient.Member.CreateInvitation(context.Background(),
		connect.NewRequest(&ownsandv1.CreateInvitationRequest{OrganizationId: org.ID, Role: ownsandv1.Role_ROLE_MEMBER}))
	if err != nil {
		t.Fatal(err)
	}
	return inv.Msg.GetInvitation().GetInvitationId(), inv.Msg.GetInvitationSecret()
}

func acceptInvitation(env *testenv.Env, user, secret string) error {
	_, err := env.User(user).Member.AcceptInvitation(context.Background(),
		connect.NewRequest(&ownsandv1.AcceptInvitationRequest{InvitationSecret: secret}))
	return err
}

func deleteOrganization(org *testenv.Org) error {
	_, err := org.OwnerClient.Organization.DeleteOrganization(context.Background(),
		connect.NewRequest(&ownsandv1.DeleteOrganizationRequest{OrganizationId: org.ID}))
	return err
}

// TestAcceptInvitationCommitsBeforeOrganizationDeletion holds the invitation
// row lock so that AcceptInvitation stays in flight, starts the organization
// deletion and releases the row. The deletion locks the organization row and
// then updates the invitations, and the membership insert of the acceptance
// needs a share of the organization row. If the acceptance locked the
// invitation first, each transaction would wait for the other: a deadlock
// (40P01) that surfaces as an internal error. The acceptance has to lock the
// organization row first, commit before the deletion, and be deleted with it.
func TestAcceptInvitationCommitsBeforeOrganizationDeletion(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	invID, secret := newInvitation(t, org)

	holder, err := env.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, "SELECT 1 FROM invitations WHERE invitation_id = $1 FOR UPDATE", invID); err != nil {
		t.Fatal(err)
	}
	acceptDone := make(chan error, 1)
	go func() { acceptDone <- acceptInvitation(env, "carol", secret) }()
	// The acceptance waits for the invitation row lock.
	waitLockWaiters(t, env, 1)
	deleteDone := make(chan error, 1)
	go func() { deleteDone <- deleteOrganization(org) }()
	// The deletion waits for the organization row that the acceptance
	// shares, or, with the wrong order, for the invitation row.
	waitLockWaiters(t, env, 2)
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	acceptErr, deleteErr := <-acceptDone, <-deleteDone
	requireNoDeadlock(t, "acceptance", acceptErr)
	requireNoDeadlock(t, "deletion", deleteErr)
	if acceptErr != nil || deleteErr != nil {
		t.Fatalf("acceptance = %v, deletion = %v; want both to succeed in order", acceptErr, deleteErr)
	}
	var memberships int
	if err := env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM memberships WHERE organization_id = $1", org.ID).Scan(&memberships); err != nil {
		t.Fatal(err)
	}
	if memberships != 0 {
		t.Fatalf("%d memberships remain after the deletion", memberships)
	}
}

// TestAcceptInvitationAfterOrganizationDeletionCommits holds the organization
// row lock as a deletion does, lets the acceptance wait for it, commits the
// deletion and expects the invitation error (not_found) and no membership.
func TestAcceptInvitationAfterOrganizationDeletionCommits(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	invID, secret := newInvitation(t, org)

	holder, err := env.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, "SELECT 1 FROM organizations WHERE organization_id = $1 FOR UPDATE", org.ID); err != nil {
		t.Fatal(err)
	}
	acceptDone := make(chan error, 1)
	go func() { acceptDone <- acceptInvitation(env, "carol", secret) }()
	waitForLockWaiter(t, env, "FROM organizations WHERE organization_id", "FOR KEY SHARE")
	// Lock the invitation row like the deletion, which would deadlock with an
	// acceptance that already holds it.
	if _, err := holder.Exec(ctx, "SELECT 1 FROM invitations WHERE invitation_id = $1 FOR UPDATE", invID); err != nil {
		t.Fatalf("lock the invitation (deadlock with the acceptance?): %v", err)
	}
	if _, err := holder.Exec(ctx, "DELETE FROM organizations WHERE organization_id = $1", org.ID); err != nil {
		t.Fatal(err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-acceptDone; testenv.Code(err) != connect.CodeNotFound {
		t.Fatalf("acceptance after the deletion: %v", err)
	}
	var memberships int
	if err := env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM memberships WHERE user_id = 'carol'").Scan(&memberships); err != nil {
		t.Fatal(err)
	}
	if memberships != 0 {
		t.Fatalf("carol has %d memberships after the refused acceptance", memberships)
	}
}

// TestConcurrentAcceptInvitationAndOrganizationDeletion starts both together
// and expects a serial outcome without a deadlock: the acceptance commits
// first and is deleted with the organization, or the deletion wins and the
// acceptance gets not_found.
func TestConcurrentAcceptInvitationAndOrganizationDeletion(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	var committedFirst, deletedFirst int
	for i := 0; i < 12; i++ {
		org := env.NewOrg("alice")
		_, secret := newInvitation(t, org)
		var acceptErr, deleteErr error
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			acceptErr = acceptInvitation(env, "carol", secret)
		}()
		go func() {
			defer wg.Done()
			<-start
			// The growing delay moves the deletion across the acceptance's
			// transaction, so both orders and the overlap occur.
			time.Sleep(time.Duration(i%6) * 300 * time.Microsecond)
			deleteErr = deleteOrganization(org)
		}()
		close(start)
		wg.Wait()

		requireNoDeadlock(t, "acceptance", acceptErr)
		requireNoDeadlock(t, "deletion", deleteErr)
		if deleteErr != nil {
			t.Fatalf("iteration %d: deletion: %v", i, deleteErr)
		}
		switch code := testenv.Code(acceptErr); {
		case acceptErr == nil:
			committedFirst++
		case code == connect.CodeNotFound:
			deletedFirst++
		default:
			t.Fatalf("iteration %d: acceptance = %v", i, acceptErr)
		}
		var rows int
		if err := env.DB.Pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM memberships WHERE organization_id = $1) + (SELECT count(*) FROM invitations WHERE organization_id = $1)", org.ID).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 0 {
			t.Fatalf("iteration %d: %d rows remain for the deleted organization", i, rows)
		}
	}
	t.Logf("acceptance committed first: %d, deletion first: %d", committedFirst, deletedFirst)
}
