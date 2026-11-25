// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
	"github.com/delinoio/mono/servers/tide-api/internal/layers"
	"github.com/delinoio/mono/servers/tide-api/internal/ownsandclient/ownsandfake"
	"github.com/delinoio/mono/servers/tide-api/internal/testenv"
)

func TestOrganizationDeletionRecoversInterruptedLinkClaim(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	// The core-only instance holds the lease to keep background work from
	// driving the fixture runners during the two deletion RPCs.
	seed := testenv.New(t)
	eventually(t, "core instance holds lease", seed.App.Lease.Held)
	fake := ownsandfake.New()
	org := seed.NewOrg("alice")
	firstLayers := &layers.Layers{}
	first := seed.NewReplica(t, testenv.WithLayers(fake, firstLayers))
	second := seed.NewReplica(t, testenv.WithLayers(fake, &layers.Layers{}))
	w := &workWorld{
		world: &world{fake: fake, l: firstLayers, env: first, org: org},
		osOrg: api.NewID(),
	}
	fake.AddOrg(w.osOrg, ownsandKey('a'))
	if _, err := w.setLink(org.Admin, w.osOrg, ownsandKey('a')); err != nil {
		t.Fatal(err)
	}
	profileID := w.createProfile(t, devProfileProto("linux-dev", 0, 0))
	queued := w.createSession(t, first.User(org.Member), profileID)
	seed.NewAPIKey(org.ID, org.Member)
	client, _, err := firstLayers.Links.Client(ctx, org.ID)
	if err != nil {
		t.Fatal(err)
	}
	pool := seed.DB.Pool
	for range 2 {
		runnerID := api.NewID()
		sb, err := client.CreateSandbox(ctx, &ownsandv1.CreateSandboxRequest{IdempotencyKey: runnerID})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO runners (runner_id, organization_id, runner_profile_id, kind, guest_os, state,
			idempotency_key, create_request, ownsand_sandbox_id, create_time, update_time)
			VALUES ($1, $2, $3, $4, $5, $6, $1, '', $7, now(), now())`,
			runnerID, org.ID, profileID, int16(tidev1.WorkloadKind_WORKLOAD_KIND_DEV_SESSION), int16(tidev1.GuestOs_GUEST_OS_LINUX),
			int16(tidev1.RunnerState_RUNNER_STATE_BUSY), sb.GetSandboxId()); err != nil {
			t.Fatal(err)
		}
	}

	var calls atomic.Int32
	entered := make(chan int32, 4)
	allowFirst, allowRetry := make(chan struct{}), make(chan struct{})
	fake.SetDeleteHook(func(ctx context.Context) error {
		n := calls.Add(1)
		var credentialsPresent bool
		err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM ownsand_links l JOIN organizations o USING (organization_id)
			WHERE l.organization_id = $1 AND length(l.api_key_ciphertext) > 0
			AND l.removing AND l.organization_deletion_attempt_id = o.deletion_attempt_id)`, org.ID).Scan(&credentialsPresent)
		if err != nil {
			return err
		}
		if !credentialsPresent {
			return errors.New("OwnSand cleanup reached the provider without the claimed credentials")
		}
		select {
		case entered <- n:
		case <-ctx.Done():
			return ctx.Err()
		}
		var allow <-chan struct{}
		switch n {
		case 1:
			allow = allowFirst
		case 2:
			allow = allowRetry
		default:
			return nil
		}
		select {
		case <-allow:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	waitCall := func(want int32) {
		t.Helper()
		select {
		case got := <-entered:
			if got != want {
				t.Fatalf("OwnSand cleanup call = %d, want %d", got, want)
			}
		case <-ctx.Done():
			t.Fatalf("OwnSand cleanup call %d did not arrive", want)
		}
	}
	remove := func(env *testenv.Env, user string) error {
		_, err := env.User(user).Organization.DeleteOrganization(ctx, connect.NewRequest(&tidev1.DeleteOrganizationRequest{OrganizationId: org.ID}))
		return err
	}
	transfer := func() error {
		_, err := second.User(org.Owner).Organization.TransferOwnership(ctx, connect.NewRequest(&tidev1.TransferOwnershipRequest{
			OrganizationId: org.ID, UserId: org.Admin,
		}))
		return err
	}

	old := make(chan error, 1)
	go func() { old <- remove(first, org.Owner) }()
	waitCall(1)
	wantCode(t, "competing linked deletion", remove(second, org.Owner), connect.CodeFailedPrecondition)
	wantCode(t, "ownership transfer during linked deletion", transfer(), connect.CodeFailedPrecondition)
	if got := w.session(t, queued.GetDevSessionId()).GetState(); got != tidev1.RequestState_REQUEST_STATE_QUEUED {
		t.Fatalf("queued session during preparation = %s", got)
	}
	var oldID string
	if err := pool.QueryRow(ctx, "SELECT deletion_attempt_id::text FROM organizations WHERE organization_id = $1", org.ID).Scan(&oldID); err != nil {
		t.Fatal(err)
	}
	var killed bool
	// The only advisory lock in this database is the active deletion
	// session's lock. Wait for Postgres to terminate that backend.
	if err := pool.QueryRow(ctx, `SELECT pg_terminate_backend(pid, 5000) FROM pg_locks
		WHERE locktype = 'advisory' AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&killed); err != nil || !killed {
		t.Fatalf("terminate the old cleanup session: %v, %v", killed, err)
	}
	wantCode(t, "admin attempts to resume deletion", remove(second, org.Admin), connect.CodePermissionDenied)
	wantCode(t, "transfer before the interrupted deletion is retried", transfer(), connect.CodeFailedPrecondition)

	retry := make(chan error, 1)
	go func() { retry <- remove(second, org.Owner) }()
	waitCall(2)
	close(allowFirst)
	if err := <-old; err == nil {
		t.Fatal("the attempt whose database session ended completed deletion")
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("the stale attempt reached another provider call: %d calls", n)
	}
	var newID, linkID string
	if err := pool.QueryRow(ctx, `SELECT o.deletion_attempt_id::text, l.organization_deletion_attempt_id::text
		FROM organizations o JOIN ownsand_links l USING (organization_id) WHERE o.organization_id = $1`, org.ID).Scan(&newID, &linkID); err != nil {
		t.Fatal(err)
	}
	if newID == oldID || linkID != newID {
		t.Fatalf("retry did not replace the old intent and take over its OwnSand link: %q, %q, %q", oldID, newID, linkID)
	}
	close(allowRetry)
	if err := <-retry; err != nil {
		t.Fatal(err)
	}
	waitCall(3)
	if fake.Active(w.osOrg) != 0 {
		t.Fatal("a sandbox survived the retry")
	}
	var orgExists bool
	var deletes, revocations int
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM organizations WHERE organization_id = $1),
		(SELECT count(*) FROM audit_events WHERE organization_id = $1 AND action = $2 AND actor_user_id = $4),
		(SELECT count(*) FROM audit_events WHERE organization_id = $1 AND action = $3)`, org.ID,
		int16(tidev1.AuditAction_AUDIT_ACTION_ORGANIZATION_DELETED), int16(tidev1.AuditAction_AUDIT_ACTION_API_KEY_REVOKED),
		org.Owner).Scan(&orgExists, &deletes, &revocations); err != nil {
		t.Fatal(err)
	}
	if orgExists || deletes != 1 || revocations != 1 {
		t.Fatalf("deletion result: organization exists = %v, deletion audits = %d, revocation audits = %d", orgExists, deletes, revocations)
	}
	t.Log("Two Tide replicas rejected competing linked requests. After PostgreSQL terminated the old session, the current owner resumed with the stored credentials. The stale attempt made no further provider call or change to the new claim. The retry deleted both sandboxes and recorded one deletion and one key revocation.")
}
