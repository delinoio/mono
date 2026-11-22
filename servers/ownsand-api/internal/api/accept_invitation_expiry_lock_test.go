// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// invitationRecords counts the rows that an acceptance writes.
type invitationRecords struct {
	memberships, audits int
	accepted            bool
}

func readInvitationRecords(t *testing.T, env *testenv.Env, orgID, invID string) invitationRecords {
	t.Helper()
	ctx := context.Background()
	var r invitationRecords
	if err := env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM memberships WHERE organization_id = $1 AND user_id = 'carol'", orgID).Scan(&r.memberships); err != nil {
		t.Fatal(err)
	}
	if err := env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE organization_id = $1 AND action = $2 AND actor_id = 'carol'",
		orgID, int16(ownsandv1.AuditAction_AUDIT_ACTION_MEMBER_JOINED)).Scan(&r.audits); err != nil {
		t.Fatal(err)
	}
	if err := env.DB.Pool.QueryRow(ctx, "SELECT accept_time IS NOT NULL FROM invitations WHERE invitation_id = $1", invID).Scan(&r.accepted); err != nil {
		t.Fatal(err)
	}
	return r
}

// acceptWhileInvitationLocked holds the invitation row lock in a test
// transaction and starts an acceptance that waits for the row. It moves the
// test clock by advance while the acceptance waits, releases the row and
// returns the acceptance result. The acceptance passed its first expiry
// check before it waited, so only an expiry check after the lock sees the
// moved clock.
func acceptWhileInvitationLocked(t *testing.T, advance time.Duration) (env *testenv.Env, orgID, invID string, acceptErr error) {
	t.Helper()
	env = testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	orgID = org.ID
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
	waitLockWaiters(t, env, 1)
	env.Clock.Advance(advance)
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	return env, orgID, invID, <-acceptDone
}

// TestAcceptInvitationRejectsInvitationExpiredDuringLockWait expires the
// invitation while the acceptance waits for the invitation row lock. The
// acceptance has to reject it and write no membership, acceptance or audit
// event.
func TestAcceptInvitationRejectsInvitationExpiredDuringLockWait(t *testing.T) {
	// The invitation lives 7 days; the lock wait crosses its expiry.
	env, orgID, invID, err := acceptWhileInvitationLocked(t, 7*24*time.Hour+time.Second)
	if code := testenv.Code(err); code != connect.CodeNotFound {
		t.Fatalf("acceptance after the invitation expired during the lock wait: %v", err)
	}
	if r := readInvitationRecords(t, env, orgID, invID); r != (invitationRecords{}) {
		t.Fatalf("records after the refused acceptance = %+v, want none", r)
	}
}

// TestAcceptInvitationSucceedsAfterLockWaitBeforeExpiry is the control: the
// same lock wait with the clock still before the expiry accepts normally.
func TestAcceptInvitationSucceedsAfterLockWaitBeforeExpiry(t *testing.T) {
	env, orgID, invID, err := acceptWhileInvitationLocked(t, time.Hour)
	if err != nil {
		t.Fatalf("acceptance after a lock wait before the expiry: %v", err)
	}
	if r := readInvitationRecords(t, env, orgID, invID); r != (invitationRecords{memberships: 1, audits: 1, accepted: true}) {
		t.Fatalf("records after the acceptance = %+v, want one membership, one audit event and an accepted invitation", r)
	}
}
