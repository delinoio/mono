// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/delinoio/mono/go/ownsand/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// waitForLockWaiter waits until a transaction whose current query contains
// every fragment waits for a lock. The fragments identify the statement that
// the test expects the RPC to block on.
func waitForLockWaiter(t *testing.T, env *testenv.Env, fragments ...string) {
	t.Helper()
	ctx := context.Background()
	query := `SELECT count(*) FROM pg_stat_activity
		WHERE datname = current_database() AND pid <> pg_backend_pid() AND wait_event_type = 'Lock'`
	args := make([]any, 0, len(fragments))
	for i, f := range fragments {
		query += fmt.Sprintf(" AND query LIKE $%d", i+1)
		args = append(args, "%"+f+"%")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := env.DB.Pool.QueryRow(ctx, query, args...).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no transaction waits for a lock on a statement with %q", fragments)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// enrollOrgLockFixture is an organization with one pending enrollment token.
type enrollOrgLockFixture struct {
	env   *testenv.Env
	org   *testenv.Org
	token string
	pub   ed25519.PublicKey
	priv  ed25519.PrivateKey
}

func newEnrollOrgLockFixture(t *testing.T, env *testenv.Env) *enrollOrgLockFixture {
	t.Helper()
	org := env.NewOrg("alice")
	resp, err := org.OwnerClient.Device.CreateEnrollmentToken(context.Background(),
		connect.NewRequest(&ownsandv1.CreateEnrollmentTokenRequest{OrganizationId: org.ID, Ttl: durationpb.New(time.Hour)}))
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &enrollOrgLockFixture{env: env, org: org, token: resp.Msg.GetEnrollmentToken(), pub: pub, priv: priv}
}

func (f *enrollOrgLockFixture) enroll() (*ownsandv1.EnrollResponse, error) {
	resp, err := f.env.Clients("").Agent.Enroll(context.Background(), connect.NewRequest(&ownsandv1.EnrollRequest{
		EnrollmentToken: f.token, PublicKey: f.pub, Name: "host", HostOs: ownsandv1.HostOs_HOST_OS_LINUX,
		Arch: ownsandv1.Arch_ARCH_AMD64, AgentVersion: "1.0.0",
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (f *enrollOrgLockFixture) device(deviceID string) *testenv.Device {
	epoch, _ := uuidv7.NewString()
	return &testenv.Device{ID: deviceID, OrgID: f.org.ID, PrivateKey: f.priv, Epoch: epoch}
}

// deviceCounts returns the number of device rows of the organization and the
// number of them that are not removed.
func (f *enrollOrgLockFixture) deviceCounts(t *testing.T) (total, active int) {
	t.Helper()
	ctx := context.Background()
	if err := f.env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM devices WHERE organization_id = $1", f.org.ID).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if err := f.env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM devices WHERE organization_id = $1 AND state <> $2",
		f.org.ID, int16(ownsandv1.DeviceState_DEVICE_STATE_REMOVED)).Scan(&active); err != nil {
		t.Fatal(err)
	}
	return total, active
}

// requireNoDeadlock fails when err is a deadlock (SQLSTATE 40P01) or another
// internal failure, which the handlers report as an internal error.
func requireNoDeadlock(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		return
	}
	if testenv.Code(err) == connect.CodeInternal || strings.Contains(err.Error(), "40P01") || strings.Contains(strings.ToLower(err.Error()), "deadlock") {
		t.Fatalf("%s failed with an internal or deadlock error: %v", what, err)
	}
}

// TestEnrollCommitsBeforeOrganizationDeletion holds the token row lock so
// that an Enroll stays in flight after it took its organization lock, starts
// the deletion, which has to wait for that lock, and lets the Enroll commit.
// The deletion must list the new device and remove it.
func TestEnrollCommitsBeforeOrganizationDeletion(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	f := newEnrollOrgLockFixture(t, env)

	var tokenID string
	if err := env.DB.Pool.QueryRow(ctx, "SELECT enrollment_token_id::text FROM enrollment_tokens WHERE organization_id = $1", f.org.ID).Scan(&tokenID); err != nil {
		t.Fatal(err)
	}
	holder, err := env.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, "SELECT 1 FROM enrollment_tokens WHERE enrollment_token_id = $1 FOR UPDATE", tokenID); err != nil {
		t.Fatal(err)
	}

	type enrolled struct {
		resp *ownsandv1.EnrollResponse
		err  error
	}
	enrollDone := make(chan enrolled, 1)
	go func() {
		resp, err := f.enroll()
		enrollDone <- enrolled{resp, err}
	}()
	// The Enroll holds the organization lock and waits for the token lock.
	waitForLockWaiter(t, env, "FROM enrollment_tokens", "FOR UPDATE")

	deleteDone := make(chan error, 1)
	go func() {
		_, err := f.org.OwnerClient.Organization.DeleteOrganization(ctx, connect.NewRequest(&ownsandv1.DeleteOrganizationRequest{OrganizationId: f.org.ID}))
		deleteDone <- err
	}()
	// The deletion waits for the organization lock of the Enroll.
	waitForOrganizationLockWaiters(t, env, 1)

	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	e := <-enrollDone
	if e.err != nil {
		t.Fatalf("enroll that committed first: %v", e.err)
	}
	if err := <-deleteDone; err != nil {
		t.Fatalf("deletion after the enroll: %v", err)
	}

	total, active := f.deviceCounts(t)
	if total != 1 || active != 0 {
		t.Fatalf("devices after the deletion: total = %d, active = %d, want 1 and 0", total, active)
	}
	resp, err := env.Authenticate(f.device(e.resp.GetDeviceId()))
	if err != nil || resp.GetRemovalNotice() == nil || resp.GetSession() != nil {
		t.Fatalf("authenticate the device that enrolled before the deletion: %v, %v", resp, err)
	}
}

// TestEnrollAfterOrganizationDeletionCommits holds the deletion's organization
// lock so that an Enroll waits for it, commits the deletion and expects the
// Enroll to fail like an unknown token and to create no device.
func TestEnrollAfterOrganizationDeletionCommits(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	f := newEnrollOrgLockFixture(t, env)

	holder, err := env.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, "SELECT 1 FROM organizations WHERE organization_id = $1 FOR UPDATE", f.org.ID); err != nil {
		t.Fatal(err)
	}
	enrollDone := make(chan error, 1)
	go func() {
		_, err := f.enroll()
		enrollDone <- err
	}()
	// The Enroll waits for the organization lock, before any token lock.
	waitForLockWaiter(t, env, "FROM organizations WHERE organization_id", "FOR KEY SHARE")

	if _, err := holder.Exec(ctx, "DELETE FROM organizations WHERE organization_id = $1", f.org.ID); err != nil {
		t.Fatal(err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-enrollDone; testenv.Code(err) != connect.CodeUnauthenticated {
		t.Fatalf("enroll after the deletion: %v", err)
	}
	if total, _ := f.deviceCounts(t); total != 0 {
		t.Fatalf("device rows after the refused enroll = %d, want 0", total)
	}
	// A later Enroll with the same token is refused too.
	if _, err := f.enroll(); testenv.Code(err) != connect.CodeUnauthenticated {
		t.Fatalf("second enroll after the deletion: %v", err)
	}
}

// TestConcurrentEnrollAndOrganizationDeletion starts both operations together
// and expects a serial outcome without a deadlock: either the Enroll commits
// first and the deletion removes the device, or the deletion wins and the
// Enroll is refused. No active device remains for the deleted organization.
func TestConcurrentEnrollAndOrganizationDeletion(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	var enrolledFirst, deletedFirst int
	for i := 0; i < 30; i++ {
		f := newEnrollOrgLockFixture(t, env)
		var enrollResp *ownsandv1.EnrollResponse
		var enrollErr, deleteErr error
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			enrollResp, enrollErr = f.enroll()
		}()
		go func() {
			defer wg.Done()
			<-start
			// The growing delay moves the deletion across the Enroll's
			// transaction, so both serial orders and the overlap occur.
			time.Sleep(time.Duration(i%10) * 100 * time.Microsecond)
			_, deleteErr = f.org.OwnerClient.Organization.DeleteOrganization(ctx, connect.NewRequest(&ownsandv1.DeleteOrganizationRequest{OrganizationId: f.org.ID}))
		}()
		close(start)
		wg.Wait()

		requireNoDeadlock(t, "enroll", enrollErr)
		requireNoDeadlock(t, "deletion", deleteErr)
		if deleteErr != nil {
			t.Fatalf("iteration %d: deletion: %v", i, deleteErr)
		}
		total, active := f.deviceCounts(t)
		if active != 0 {
			t.Fatalf("iteration %d: %d active devices remain for the deleted organization", i, active)
		}
		switch {
		case enrollErr == nil:
			enrolledFirst++
			if total != 1 {
				t.Fatalf("iteration %d: enroll committed first but %d device rows exist", i, total)
			}
			resp, err := env.Authenticate(f.device(enrollResp.GetDeviceId()))
			if err != nil || resp.GetRemovalNotice() == nil || resp.GetSession() != nil {
				t.Fatalf("iteration %d: authenticate after the deletion: %v, %v", i, resp, err)
			}
		case testenv.Code(enrollErr) == connect.CodeUnauthenticated:
			deletedFirst++
			if total != 0 {
				t.Fatalf("iteration %d: deletion committed first but %d device rows exist", i, total)
			}
		default:
			t.Fatalf("iteration %d: enroll = %v", i, enrollErr)
		}
	}
	t.Logf("enroll first: %d, deletion first: %d", enrolledFirst, deletedFirst)
}
