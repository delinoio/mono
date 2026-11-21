// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// createOrgLockFixture is an organization with a connected device, a running
// sandbox and a READY snapshot of it: the inputs of CreateSandbox,
// CreateSnapshot and CreateTemplate.
type createOrgLockFixture struct {
	env        *testenv.Env
	org        *testenv.Org
	sandboxID  string
	snapshotID string
}

func newCreateOrgLockFixture(t *testing.T, env *testenv.Env) *createOrgLockFixture {
	t.Helper()
	org := env.NewOrg("alice")
	setupAgent(t, env, org, nil)
	created, err := org.OwnerClient.Sandbox.CreateSandbox(context.Background(), connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	sandboxID := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, org.OwnerClient, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	return &createOrgLockFixture{env: env, org: org, sandboxID: sandboxID, snapshotID: plainSnapshot(t, org, sandboxID)}
}

// createRPC is one of the RPCs that insert a row with a foreign key to the
// organization after they locked child rows. childLock is the statement that
// locks the child row that the RPC locks first; the test holds that lock.
type createRPC struct {
	name      string
	childLock func(f *createOrgLockFixture) (sql string, args []any)
	call      func(f *createOrgLockFixture) error
}

var createRPCs = []createRPC{
	{
		name: "CreateSandbox",
		childLock: func(f *createOrgLockFixture) (string, []any) {
			return "SELECT 1 FROM devices WHERE organization_id = $1 FOR UPDATE", []any{f.org.ID}
		},
		call: func(f *createOrgLockFixture) error {
			_, err := f.org.OwnerClient.Sandbox.CreateSandbox(context.Background(), connect.NewRequest(createReq(f.org.ID)))
			return err
		},
	},
	{
		name: "CreateSnapshot",
		childLock: func(f *createOrgLockFixture) (string, []any) {
			return "SELECT 1 FROM sandboxes WHERE sandbox_id = $1 FOR UPDATE", []any{f.sandboxID}
		},
		call: func(f *createOrgLockFixture) error {
			_, err := f.org.OwnerClient.Snapshot.CreateSnapshot(context.Background(),
				connect.NewRequest(&ownsandv1.CreateSnapshotRequest{OrganizationId: f.org.ID, SandboxId: f.sandboxID}))
			return err
		},
	},
	{
		name: "CreateTemplate",
		childLock: func(f *createOrgLockFixture) (string, []any) {
			return "SELECT 1 FROM snapshots WHERE snapshot_id = $1 FOR UPDATE", []any{f.snapshotID}
		},
		call: func(f *createOrgLockFixture) error {
			_, err := f.org.OwnerClient.Snapshot.CreateTemplate(context.Background(),
				connect.NewRequest(&ownsandv1.CreateTemplateRequest{OrganizationId: f.org.ID, SnapshotId: f.snapshotID, Name: "tpl"}))
			return err
		},
	},
}

// TestCreateRPCCommitsBeforeOrganizationDeletion holds the child row lock that
// the RPC takes first, so that the RPC stays in flight, starts
// DeleteOrganization and releases the child row. The deletion locks the
// organization row and then the child rows, and the RPC's insert needs a
// share of the organization row. If the RPC locked its child rows first,
// each transaction would wait for the other: a deadlock (40P01) that
// surfaces as an internal error. The RPC has to lock the organization row
// first and commit before the deletion, which then deletes the new row.
func TestCreateRPCCommitsBeforeOrganizationDeletion(t *testing.T) {
	for _, rpc := range createRPCs {
		t.Run(rpc.name, func(t *testing.T) {
			env := testenv.New(t)
			ctx := context.Background()
			f := newCreateOrgLockFixture(t, env)

			holder, err := env.DB.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = holder.Rollback(ctx) }()
			sql, args := rpc.childLock(f)
			if _, err := holder.Exec(ctx, sql, args...); err != nil {
				t.Fatal(err)
			}

			rpcDone := make(chan error, 1)
			go func() { rpcDone <- rpc.call(f) }()
			// The RPC waits for the child row lock.
			waitLockWaiters(t, env, 1)
			deleteDone := make(chan error, 1)
			go func() {
				_, err := f.org.OwnerClient.Organization.DeleteOrganization(ctx,
					connect.NewRequest(&ownsandv1.DeleteOrganizationRequest{OrganizationId: f.org.ID}))
				deleteDone <- err
			}()
			// The deletion waits too: for the organization row that the
			// RPC shares, or, with the wrong order, for the child row.
			waitLockWaiters(t, env, 2)
			if err := holder.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			rpcErr := <-rpcDone
			deleteErr := <-deleteDone
			requireNoDeadlock(t, rpc.name, rpcErr)
			requireNoDeadlock(t, "deletion", deleteErr)
			if deleteErr != nil {
				t.Fatalf("deletion: %v", deleteErr)
			}
			// The RPC committed first (or, for the commands that it sends
			// after its commit, failed once the deletion closed the agent
			// channel). It never reports not_found here: its lock came first.
			if code := testenv.Code(rpcErr); rpcErr != nil && code != connect.CodeUnavailable {
				t.Fatalf("%s: %v", rpc.name, rpcErr)
			}
			var remaining int
			if err := env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM organizations WHERE organization_id = $1", f.org.ID).Scan(&remaining); err != nil {
				t.Fatal(err)
			}
			if remaining != 0 {
				t.Fatalf("organization rows after the deletion = %d, want 0", remaining)
			}
		})
	}
}

// TestCreateRPCAfterOrganizationDeletionCommits holds the organization row
// lock as a deletion does, lets the RPC wait for it, commits the deletion
// and expects not_found. The RPC has to wait on the organization row before
// any child row.
func TestCreateRPCAfterOrganizationDeletionCommits(t *testing.T) {
	for _, rpc := range createRPCs {
		t.Run(rpc.name, func(t *testing.T) {
			env := testenv.New(t)
			ctx := context.Background()
			f := newCreateOrgLockFixture(t, env)

			// Delete the organization in a transaction that the test
			// commits later: the real handler cannot be paused midway.
			holder, err := env.DB.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = holder.Rollback(ctx) }()
			if _, err := holder.Exec(ctx, "SELECT 1 FROM organizations WHERE organization_id = $1 FOR UPDATE", f.org.ID); err != nil {
				t.Fatal(err)
			}
			rpcDone := make(chan error, 1)
			go func() { rpcDone <- rpc.call(f) }()
			waitForLockWaiter(t, env, "FROM organizations WHERE organization_id", "FOR KEY SHARE")

			// Lock the child rows like the deletion, which would deadlock
			// with an RPC that already holds one of them.
			for _, stmt := range []string{
				"SELECT 1 FROM devices WHERE organization_id = $1 FOR UPDATE",
				"SELECT 1 FROM sandboxes WHERE organization_id = $1 FOR UPDATE",
				"SELECT 1 FROM snapshots WHERE organization_id = $1 FOR UPDATE",
			} {
				if _, err := holder.Exec(ctx, stmt, f.org.ID); err != nil {
					t.Fatalf("lock child rows (deadlock with the RPC?): %v", err)
				}
			}
			for _, table := range []string{"templates", "snapshots", "sandbox_create_keys", "sandboxes"} {
				if _, err := holder.Exec(ctx, fmt.Sprintf("DELETE FROM %s WHERE organization_id = $1", table), f.org.ID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := holder.Exec(ctx, "DELETE FROM devices WHERE organization_id = $1", f.org.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := holder.Exec(ctx, "DELETE FROM organizations WHERE organization_id = $1", f.org.ID); err != nil {
				t.Fatal(err)
			}
			if err := holder.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-rpcDone; testenv.Code(err) != connect.CodeNotFound {
				t.Fatalf("%s after the deletion: %v", rpc.name, err)
			}
		})
	}
}

// TestConcurrentCreateRPCAndOrganizationDeletion starts an RPC and the
// deletion together and expects a serial outcome without a deadlock: the RPC
// either commits first, or the deletion wins and the RPC gets not_found.
func TestConcurrentCreateRPCAndOrganizationDeletion(t *testing.T) {
	for _, rpc := range createRPCs {
		t.Run(rpc.name, func(t *testing.T) {
			env := testenv.New(t)
			ctx := context.Background()
			var committedFirst, deletedFirst int
			for i := 0; i < 8; i++ {
				f := newCreateOrgLockFixture(t, env)
				var rpcErr, deleteErr error
				start := make(chan struct{})
				var wg sync.WaitGroup
				wg.Add(2)
				go func() {
					defer wg.Done()
					<-start
					rpcErr = rpc.call(f)
				}()
				go func() {
					defer wg.Done()
					<-start
					// The growing delay moves the deletion across the
					// RPC's transaction, so both orders and the overlap occur.
					time.Sleep(time.Duration(i%8) * 300 * time.Microsecond)
					_, deleteErr = f.org.OwnerClient.Organization.DeleteOrganization(ctx,
						connect.NewRequest(&ownsandv1.DeleteOrganizationRequest{OrganizationId: f.org.ID}))
				}()
				close(start)
				wg.Wait()

				requireNoDeadlock(t, rpc.name, rpcErr)
				requireNoDeadlock(t, "deletion", deleteErr)
				if deleteErr != nil {
					t.Fatalf("iteration %d: deletion: %v", i, deleteErr)
				}
				switch code := testenv.Code(rpcErr); {
				case rpcErr == nil || code == connect.CodeUnavailable:
					committedFirst++
				case code == connect.CodeNotFound:
					deletedFirst++
				default:
					t.Fatalf("iteration %d: %s = %v", i, rpc.name, rpcErr)
				}
				var rows int
				if err := env.DB.Pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM sandboxes WHERE organization_id = $1) + (SELECT count(*) FROM snapshots WHERE organization_id = $1) + (SELECT count(*) FROM templates WHERE organization_id = $1)", f.org.ID).Scan(&rows); err != nil {
					t.Fatal(err)
				}
				if rows != 0 {
					t.Fatalf("iteration %d: %d rows remain for the deleted organization", i, rows)
				}
			}
			t.Logf("%s committed first: %d, deletion first: %d", rpc.name, committedFirst, deletedFirst)
		})
	}
}
