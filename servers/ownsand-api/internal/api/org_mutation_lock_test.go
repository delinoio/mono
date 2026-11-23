// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// orgMutationRPC is an RPC that inserts one row with a foreign key to the
// organization in the transaction that authorized the caller, without
// locking any child row first. table is the table of the row.
type orgMutationRPC struct {
	name  string
	table string
	call  func(org *testenv.Org) error
}

func policyRPC(name string, mode ownsandv1.AgentVersionPolicyMode) orgMutationRPC {
	return orgMutationRPC{
		name: name, table: "agent_version_policies",
		call: func(org *testenv.Org) error {
			_, err := org.OwnerClient.Device.UpdateAgentVersionPolicy(context.Background(), connect.NewRequest(
				&ownsandv1.UpdateAgentVersionPolicyRequest{OrganizationId: org.ID, Mode: mode}))
			return err
		},
	}
}

var orgMutationRPCs = []orgMutationRPC{
	{
		name: "CreateInvitation", table: "invitations",
		call: func(org *testenv.Org) error {
			_, err := org.OwnerClient.Member.CreateInvitation(context.Background(), connect.NewRequest(
				&ownsandv1.CreateInvitationRequest{OrganizationId: org.ID, Role: ownsandv1.Role_ROLE_MEMBER}))
			return err
		},
	},
	{
		name: "CreateEnrollmentToken", table: "enrollment_tokens",
		call: func(org *testenv.Org) error {
			_, err := org.OwnerClient.Device.CreateEnrollmentToken(context.Background(), connect.NewRequest(
				&ownsandv1.CreateEnrollmentTokenRequest{OrganizationId: org.ID, Ttl: durationpb.New(time.Hour)}))
			return err
		},
	},
	policyRPC("UpdateAgentVersionPolicy", ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_HOLD),
	{
		name: "CreateApiKey", table: "api_keys",
		call: func(org *testenv.Org) error {
			_, err := org.OwnerClient.APIKey.CreateApiKey(context.Background(), connect.NewRequest(
				&ownsandv1.CreateApiKeyRequest{OrganizationId: org.ID, Name: "late"}))
			return err
		},
	},
}

// orgRecordCounts counts the rows that a create RPC of the organization
// writes: the table of the RPC, the audit events and the control events.
func orgRecordCounts(t *testing.T, env *testenv.Env, orgID, table string) [3]int {
	t.Helper()
	return [3]int{
		countRows(t, env, fmt.Sprintf("SELECT count(*) FROM %s WHERE organization_id = $1", table), orgID),
		countRows(t, env, "SELECT count(*) FROM audit_events WHERE organization_id = $1", orgID),
		countRows(t, env, "SELECT count(*) FROM control_events WHERE organization_id = $1", orgID),
	}
}

// holdOrganizationDeletion deletes the organization in a transaction that
// stays open, as DeleteOrganization does: it locks the organization row
// first and the foreign keys cascade to the other records. The returned
// function commits the deletion.
func holdOrganizationDeletion(t *testing.T, env *testenv.Env, orgID string) func() {
	t.Helper()
	ctx := context.Background()
	holder, err := env.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Rollback(ctx) })
	if _, err := holder.Exec(ctx, "SELECT 1 FROM organizations WHERE organization_id = $1 FOR UPDATE", orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, "DELETE FROM organizations WHERE organization_id = $1", orgID); err != nil {
		t.Fatal(err)
	}
	return func() {
		t.Helper()
		if err := holder.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

// TestCreateRPCAfterInFlightOrganizationDeletionCommits holds a deletion of
// the organization open, lets each create RPC reach its insert, commits the
// deletion and expects not_found. Without the organization lock, the
// insert waited for the deletion and then failed with a foreign key
// violation (23503) that surfaced as an internal error. The test also
// requires that the server logged no internal error and that the failed RPC
// left no record and no audit or control event. UpdateAgentVersionPolicy
// runs with and without a stored policy row.
func TestCreateRPCAfterInFlightOrganizationDeletionCommits(t *testing.T) {
	rpcs := append([]orgMutationRPC{}, orgMutationRPCs...)
	rpcs = append(rpcs, policyRPC("UpdateAgentVersionPolicyWithExistingRow", ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_HOLD))
	for _, rpc := range rpcs {
		t.Run(rpc.name, func(t *testing.T) {
			env := testenv.New(t)
			org := env.NewOrg("alice")
			if strings.HasSuffix(rpc.name, "WithExistingRow") {
				// A stored policy row makes the upsert take its conflict path.
				if _, err := env.DB.Pool.Exec(context.Background(),
					"INSERT INTO agent_version_policies (organization_id, mode, pinned_version, update_time, updated_by_kind, updated_by_id) VALUES ($1, 1, '', now(), 1, 'x')",
					org.ID); err != nil {
					t.Fatal(err)
				}
			}
			table := rpc.table
			// Audit and control events keep no foreign key to the organization,
			// so a stray event written by the losing RPC would outlive the
			// deletion. Rows that exist before the RPC (for example from
			// NewOrg) stay, so the check compares the counts before and after.
			before := orgRecordCounts(t, env, org.ID, table)
			commit := holdOrganizationDeletion(t, env, org.ID)

			rpcDone := make(chan error, 1)
			go func() { rpcDone <- rpc.call(org) }()
			// The RPC waits for the deletion: for the organization row in its
			// lock, or, in an unlocked order, in its insert.
			waitLockWaiters(t, env, 1)
			commit()
			err := <-rpcDone
			if testenv.Code(err) != connect.CodeNotFound {
				t.Fatalf("%s after the deletion: %v", rpc.name, err)
			}
			if strings.Contains(env.Logs.String(), "internal_error") {
				t.Fatalf("the server logged an internal error:\n%s", env.Logs.String())
			}
			after := orgRecordCounts(t, env, org.ID, table)
			if after[0] != 0 {
				t.Fatalf("%s rows of the deleted organization = %d, want 0", table, after[0])
			}
			if after[1] != before[1] {
				t.Fatalf("audit_events of the deleted organization = %d, want %d (the count before the RPC)", after[1], before[1])
			}
			if after[2] != before[2] {
				t.Fatalf("control_events of the deleted organization = %d, want %d (the count before the RPC)", after[2], before[2])
			}
		})
	}
}

// TestCreateRPCKeepsAuthorizationPrecedenceDuringDeletion holds a deletion
// open and expects a caller who is not allowed to answer at once with
// permission_denied or not_found, without waiting for the deletion: the
// authorization runs before the lock.
func TestCreateRPCKeepsAuthorizationPrecedenceDuringDeletion(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	commit := holdOrganizationDeletion(t, env, org.ID)
	defer commit()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	member := env.User(org.Member)
	outsider := env.User("mallory")
	for _, tc := range []struct {
		name string
		call func() error
		want connect.Code
	}{
		{"member CreateInvitation", func() error {
			_, err := member.Member.CreateInvitation(ctx, connect.NewRequest(&ownsandv1.CreateInvitationRequest{OrganizationId: org.ID, Role: ownsandv1.Role_ROLE_MEMBER}))
			return err
		}, connect.CodePermissionDenied},
		{"outsider CreateEnrollmentToken", func() error {
			_, err := outsider.Device.CreateEnrollmentToken(ctx, connect.NewRequest(&ownsandv1.CreateEnrollmentTokenRequest{OrganizationId: org.ID, Ttl: durationpb.New(time.Hour)}))
			return err
		}, connect.CodeNotFound},
		{"member UpdateAgentVersionPolicy", func() error {
			_, err := member.Device.UpdateAgentVersionPolicy(ctx, connect.NewRequest(&ownsandv1.UpdateAgentVersionPolicyRequest{OrganizationId: org.ID, Mode: ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_HOLD}))
			return err
		}, connect.CodePermissionDenied},
		{"outsider CreateApiKey", func() error {
			_, err := outsider.APIKey.CreateApiKey(ctx, connect.NewRequest(&ownsandv1.CreateApiKeyRequest{OrganizationId: org.ID, Name: "x"}))
			return err
		}, connect.CodeNotFound},
	} {
		if got := testenv.Code(tc.call()); got != tc.want {
			t.Errorf("%s: code = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestCreateRPCWithoutDeletionIsUnchanged runs every RPC against an
// organization that nobody deletes and expects success and its record.
func TestCreateRPCWithoutDeletionIsUnchanged(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	for _, rpc := range orgMutationRPCs {
		before := orgRecordCounts(t, env, org.ID, rpc.table)
		if err := rpc.call(org); err != nil {
			t.Fatalf("%s: %v", rpc.name, err)
		}
		after := orgRecordCounts(t, env, org.ID, rpc.table)
		if after[1] != before[1]+1 {
			t.Errorf("%s: audit events %d -> %d, want one more", rpc.name, before[1], after[1])
		}
		if rpc.table == "agent_version_policies" {
			if after[0] != 1 || after[2] != before[2]+1 {
				t.Errorf("%s: policy rows %d, control events %d -> %d", rpc.name, after[0], before[2], after[2])
			}
		} else if after[0] != before[0]+1 {
			t.Errorf("%s: %s rows %d -> %d, want one more", rpc.name, rpc.table, before[0], after[0])
		}
	}
}

// TestConcurrentCreateRPCAndInFlightDeletion starts each RPC and the
// deletion together, with the deletion delayed across the RPC's
// transaction, and expects only success or not_found: never an internal
// error from a foreign key violation or a deadlock.
func TestConcurrentCreateRPCAndInFlightDeletion(t *testing.T) {
	for _, rpc := range orgMutationRPCs {
		t.Run(rpc.name, func(t *testing.T) {
			env := testenv.New(t)
			ctx := context.Background()
			var succeeded, notFound int
			for i := 0; i < 8; i++ {
				org := env.NewOrg("alice")
				var rpcErr, deleteErr error
				start := make(chan struct{})
				var wg sync.WaitGroup
				wg.Add(2)
				go func() {
					defer wg.Done()
					<-start
					rpcErr = rpc.call(org)
				}()
				go func() {
					defer wg.Done()
					<-start
					time.Sleep(time.Duration(i) * 300 * time.Microsecond)
					_, deleteErr = org.OwnerClient.Organization.DeleteOrganization(ctx,
						connect.NewRequest(&ownsandv1.DeleteOrganizationRequest{OrganizationId: org.ID}))
				}()
				close(start)
				wg.Wait()
				if deleteErr != nil {
					t.Fatalf("iteration %d: deletion: %v", i, deleteErr)
				}
				switch {
				case rpcErr == nil:
					succeeded++
				case testenv.Code(rpcErr) == connect.CodeNotFound:
					notFound++
				default:
					t.Fatalf("iteration %d: %s = %v, want success or not_found", i, rpc.name, rpcErr)
				}
				if got := countRows(t, env, fmt.Sprintf("SELECT count(*) FROM %s WHERE organization_id = $1", rpc.table), org.ID); got != 0 {
					t.Fatalf("iteration %d: %d %s rows remain for the deleted organization", i, got, rpc.table)
				}
			}
			if strings.Contains(env.Logs.String(), "internal_error") {
				t.Fatalf("the server logged an internal error:\n%s", env.Logs.String())
			}
			t.Logf("%s succeeded: %d, not_found: %d", rpc.name, succeeded, notFound)
		})
	}
}
