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

// waitForOrganizationLockWaiters waits until count transactions wait for the
// row lock of an organization. The test holds that lock in its own
// transaction, so a waiter is an RPC that passed its pre-check and is
// blocked before the authoritative owner check.
func waitForOrganizationLockWaiters(t *testing.T, env *testenv.Env, count int) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := env.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'
			AND query LIKE '%FROM organizations WHERE organization_id%FOR UPDATE%'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= count {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d RPCs wait for the organization lock, want %d", waiting, count)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type orgCounts struct {
	organizations, activeDevices, deletionAudits, transferAudits, cleanupJobs int
	ownerRole                                                                 ownsandv1.Role
	memberRole                                                                ownsandv1.Role
}

func readOrgCounts(t *testing.T, env *testenv.Env, org *testenv.Org) orgCounts {
	t.Helper()
	ctx := context.Background()
	var c orgCounts
	one := func(dst any, sql string, args ...any) {
		t.Helper()
		if err := env.DB.Pool.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	one(&c.organizations, "SELECT count(*) FROM organizations WHERE organization_id = $1", org.ID)
	one(&c.activeDevices, "SELECT count(*) FROM devices WHERE organization_id = $1 AND state <> $2", org.ID, int16(ownsandv1.DeviceState_DEVICE_STATE_REMOVED))
	one(&c.deletionAudits, "SELECT count(*) FROM audit_events WHERE organization_id = $1 AND action = $2", org.ID, int16(ownsandv1.AuditAction_AUDIT_ACTION_ORGANIZATION_DELETED))
	one(&c.transferAudits, "SELECT count(*) FROM audit_events WHERE organization_id = $1 AND action = $2", org.ID, int16(ownsandv1.AuditAction_AUDIT_ACTION_OWNERSHIP_TRANSFERRED))
	one(&c.cleanupJobs, "SELECT count(*) FROM r2_cleanup_jobs WHERE organization_id = $1", org.ID)
	// Roles are only meaningful while the organization exists.
	for user, dst := range map[string]*ownsandv1.Role{org.Owner: &c.ownerRole, org.Member: &c.memberRole} {
		var role int16
		err := env.DB.Pool.QueryRow(ctx, "SELECT role FROM memberships WHERE organization_id = $1 AND user_id = $2", org.ID, user).Scan(&role)
		if err == nil {
			*dst = ownsandv1.Role(role)
		}
	}
	return c
}

// TestDeleteOrganizationAfterOwnershipTransfer reproduces a deletion whose
// owner check read the memberships before a transfer committed: the test
// holds the organization lock, lets the former owner's deletion block on it,
// commits the transfer and expects the deletion to see the new roles.
func TestDeleteOrganizationAfterOwnershipTransfer(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_LINUX, ownsandv1.Arch_ARCH_AMD64)

	holder, err := env.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, "SELECT 1 FROM organizations WHERE organization_id = $1 FOR UPDATE", org.ID); err != nil {
		t.Fatal(err)
	}
	deleted := make(chan error, 1)
	go func() {
		_, err := org.OwnerClient.Organization.DeleteOrganization(ctx, connect.NewRequest(&ownsandv1.DeleteOrganizationRequest{OrganizationId: org.ID}))
		deleted <- err
	}()
	waitForOrganizationLockWaiters(t, env, 1)
	// The transfer commits while the deletion waits.
	if _, err := holder.Exec(ctx, "UPDATE memberships SET role = $3 WHERE organization_id = $1 AND user_id = $2", org.ID, org.Owner, int16(ownsandv1.Role_ROLE_ADMIN)); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, "UPDATE memberships SET role = $3 WHERE organization_id = $1 AND user_id = $2", org.ID, org.Member, int16(ownsandv1.Role_ROLE_OWNER)); err != nil {
		t.Fatal(err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-deleted; testenv.Code(err) != connect.CodePermissionDenied {
		t.Fatalf("deletion by the former owner: %v", err)
	}
	c := readOrgCounts(t, env, org)
	if c.organizations != 1 || c.activeDevices != 1 || c.deletionAudits != 0 || c.cleanupJobs != 0 ||
		c.ownerRole != ownsandv1.Role_ROLE_ADMIN || c.memberRole != ownsandv1.Role_ROLE_OWNER {
		t.Fatalf("state after the refused deletion: %+v", c)
	}
}

// TestTransferOwnershipAfterOrganizationDeletion is the opposite order: a
// transfer that waits for the organization lock while a deletion commits
// gets not_found and writes nothing.
func TestTransferOwnershipAfterOrganizationDeletion(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")

	holder, err := env.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, "SELECT 1 FROM organizations WHERE organization_id = $1 FOR UPDATE", org.ID); err != nil {
		t.Fatal(err)
	}
	transferred := make(chan error, 1)
	go func() {
		_, err := org.OwnerClient.Organization.TransferOwnership(ctx, connect.NewRequest(&ownsandv1.TransferOwnershipRequest{OrganizationId: org.ID, UserId: org.Member}))
		transferred <- err
	}()
	waitForOrganizationLockWaiters(t, env, 1)
	// The deletion commits while the transfer waits.
	if _, err := holder.Exec(ctx, "DELETE FROM organizations WHERE organization_id = $1", org.ID); err != nil {
		t.Fatal(err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-transferred; testenv.Code(err) != connect.CodeNotFound {
		t.Fatalf("transfer after deletion: %v", err)
	}
	if c := readOrgCounts(t, env, org); c.organizations != 0 || c.transferAudits != 0 {
		t.Fatalf("state after the refused transfer: %+v", c)
	}
}

// TestConcurrentDeleteAndTransfer fires both owner-only mutations at once and
// expects one of the two serial outcomes.
func TestConcurrentDeleteAndTransfer(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		org := env.NewOrg("alice")
		env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_LINUX, ownsandv1.Arch_ARCH_AMD64)
		var deleteErr, transferErr error
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, deleteErr = org.OwnerClient.Organization.DeleteOrganization(ctx, connect.NewRequest(&ownsandv1.DeleteOrganizationRequest{OrganizationId: org.ID}))
		}()
		go func() {
			defer wg.Done()
			<-start
			_, transferErr = org.OwnerClient.Organization.TransferOwnership(ctx, connect.NewRequest(&ownsandv1.TransferOwnershipRequest{OrganizationId: org.ID, UserId: org.Member}))
		}()
		close(start)
		wg.Wait()
		c := readOrgCounts(t, env, org)
		switch {
		case deleteErr == nil && testenv.Code(transferErr) == connect.CodeNotFound:
			// The deletion committed first.
			if c.organizations != 0 || c.deletionAudits != 1 || c.cleanupJobs != 1 || c.transferAudits != 0 {
				t.Fatalf("iteration %d, deletion first: %+v", i, c)
			}
		case transferErr == nil && testenv.Code(deleteErr) == connect.CodePermissionDenied:
			// The transfer committed first.
			if c.organizations != 1 || c.activeDevices != 1 || c.deletionAudits != 0 || c.cleanupJobs != 0 || c.transferAudits != 1 ||
				c.ownerRole != ownsandv1.Role_ROLE_ADMIN || c.memberRole != ownsandv1.Role_ROLE_OWNER {
				t.Fatalf("iteration %d, transfer first: %+v", i, c)
			}
		default:
			t.Fatalf("iteration %d is not serial: delete = %v, transfer = %v, state = %+v", i, deleteErr, transferErr, c)
		}
	}
}
