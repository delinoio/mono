// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/testenv"
)

func TestPersonalAPIKeyLifecycle(t *testing.T) {
	env := testenv.New(t, withFakeDevSessions())
	ctx := context.Background()
	org := env.NewOrg("alice")
	member := env.User(org.Member)

	resp, err := member.APIKey.CreateApiKey(ctx, connect.NewRequest(&tidev1.CreateApiKeyRequest{OrganizationId: org.ID, Name: "claude"}))
	if err != nil {
		t.Fatal(err)
	}
	secret, key := resp.Msg.GetSecret(), resp.Msg.GetApiKey()
	if !strings.HasPrefix(secret, "tide_") || len(secret) != 5+43 || key.GetDisplayPrefix() != secret[:9] ||
		key.GetState() != tidev1.ApiKeyState_API_KEY_STATE_ACTIVE || key.GetExpiresAt() != nil || key.GetLastUsedAt() != nil {
		t.Fatalf("key %v / secret of %d bytes", key, len(secret))
	}
	// The secret is not stored; only its hash is.
	var plain int
	_ = env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM api_keys WHERE position($1::bytea in secret_sha256) > 0", []byte(secret)).Scan(&plain)
	if plain != 0 {
		t.Fatal("secret stored")
	}

	for _, name := range []string{"", strings.Repeat("k", 65)} {
		_, err := member.APIKey.CreateApiKey(ctx, connect.NewRequest(&tidev1.CreateApiKeyRequest{OrganizationId: org.ID, Name: name}))
		wantCode(t, "name "+name, err, connect.CodeInvalidArgument)
	}
	_, err = member.APIKey.CreateApiKey(ctx, connect.NewRequest(&tidev1.CreateApiKeyRequest{OrganizationId: org.ID, Name: "past", ExpiresAt: timestamppb.New(env.Clock.Now().Add(-time.Second))}))
	wantCode(t, "past expiry", err, connect.CodeInvalidArgument)
	_, err = member.APIKey.CreateApiKey(ctx, connect.NewRequest(&tidev1.CreateApiKeyRequest{
		OrganizationId: org.ID, Name: "malformed", ExpiresAt: &timestamppb.Timestamp{Seconds: env.Clock.Now().Unix(), Nanos: -1},
	}))
	wantCode(t, "malformed expiry", err, connect.CodeInvalidArgument)

	// The key works on DevSessionService and nowhere else.
	keyClient := env.Bearer(secret)
	if err := listProfiles(keyClient, org.ID); err != nil {
		t.Fatalf("dev session RPC with key: %v", err)
	}
	_, err = keyClient.Organization.GetOrganization(ctx, connect.NewRequest(&tidev1.GetOrganizationRequest{OrganizationId: org.ID}))
	wantCode(t, "GetOrganization with key", err, connect.CodePermissionDenied)
	_, err = keyClient.APIKey.ListApiKeys(ctx, connect.NewRequest(&tidev1.ListApiKeysRequest{OrganizationId: org.ID}))
	wantCode(t, "ListApiKeys with key", err, connect.CodePermissionDenied)
	// Another organization's ID: not_found.
	other := env.NewOrg("bob")
	wantCode(t, "key with another organization", listProfiles(keyClient, other.ID), connect.CodeNotFound)

	// Last use: written once, not again within a minute.
	lastUsed := func() *time.Time {
		var t2 *time.Time
		if err := env.DB.Pool.QueryRow(ctx, "SELECT last_used_time FROM api_keys WHERE api_key_id = $1", key.GetApiKeyId()).Scan(&t2); err != nil {
			t.Fatal(err)
		}
		return t2
	}
	first := lastUsed()
	if first == nil || !first.Equal(env.Clock.Now()) {
		t.Fatalf("last use %v, want %v", first, env.Clock.Now())
	}
	env.Clock.Advance(30 * time.Second)
	_ = listProfiles(keyClient, org.ID)
	if got := lastUsed(); !got.Equal(*first) {
		t.Fatalf("last use moved within a minute: %v", got)
	}
	env.Clock.Advance(31 * time.Second)
	_ = listProfiles(keyClient, org.ID)
	if got := lastUsed(); !got.After(*first) {
		t.Fatalf("last use not updated after a minute: %v", got)
	}

	// Each user lists and revokes only their own keys.
	_, aliceSecret := env.NewAPIKey(org.ID, org.Owner)
	list, err := member.APIKey.ListApiKeys(ctx, connect.NewRequest(&tidev1.ListApiKeysRequest{OrganizationId: org.ID}))
	if err != nil || len(list.Msg.GetApiKeys()) != 1 || list.Msg.GetApiKeys()[0].GetApiKeyId() != key.GetApiKeyId() || list.Msg.GetApiKeys()[0].GetLastUsedAt() == nil {
		t.Fatalf("member keys: %v %v", list, err)
	}
	ownerKeys, _ := env.User(org.Owner).APIKey.ListApiKeys(ctx, connect.NewRequest(&tidev1.ListApiKeysRequest{OrganizationId: org.ID}))
	_, err = env.User(org.Owner).APIKey.RevokeApiKey(ctx, connect.NewRequest(&tidev1.RevokeApiKeyRequest{OrganizationId: org.ID, ApiKeyId: key.GetApiKeyId()}))
	wantCode(t, "owner revokes a member key", err, connect.CodeNotFound)
	if len(ownerKeys.Msg.GetApiKeys()) != 1 || ownerKeys.Msg.GetApiKeys()[0].GetApiKeyId() == key.GetApiKeyId() {
		t.Fatalf("owner keys: %v", ownerKeys)
	}

	rev, err := member.APIKey.RevokeApiKey(ctx, connect.NewRequest(&tidev1.RevokeApiKeyRequest{OrganizationId: org.ID, ApiKeyId: key.GetApiKeyId()}))
	if err != nil || rev.Msg.GetApiKey().GetState() != tidev1.ApiKeyState_API_KEY_STATE_REVOKED || rev.Msg.GetApiKey().GetRevokedAt() == nil {
		t.Fatalf("revoke: %v %v", rev, err)
	}
	if _, err := member.APIKey.RevokeApiKey(ctx, connect.NewRequest(&tidev1.RevokeApiKeyRequest{OrganizationId: org.ID, ApiKeyId: key.GetApiKeyId()})); err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	wantCode(t, "revoked key", listProfiles(keyClient, org.ID), connect.CodeUnauthenticated)
	if err := listProfiles(env.Bearer(aliceSecret), org.ID); err != nil {
		t.Fatalf("owner key: %v", err)
	}

	// Wrong shapes and unknown keys are unauthenticated, also on RPCs that
	// reject keys.
	for _, bad := range []string{"tide_short", "tide_" + strings.Repeat("A", 43), secret + "x"} {
		wantCode(t, "bad key "+bad, listProfiles(env.Bearer(bad), org.ID), connect.CodeUnauthenticated)
		_, err := env.Bearer(bad).Organization.ListOrganizations(ctx, connect.NewRequest(&tidev1.ListOrganizationsRequest{}))
		wantCode(t, "bad key on ListOrganizations", err, connect.CodeUnauthenticated)
	}

	actions := auditActions(t, env, org.ID)
	if countAction(actions, tidev1.AuditAction_AUDIT_ACTION_API_KEY_CREATED) != 2 || countAction(actions, tidev1.AuditAction_AUDIT_ACTION_API_KEY_REVOKED) != 1 {
		t.Fatalf("audit %v", actions)
	}
}

func TestPersonalAPIKeyExpiry(t *testing.T) {
	env := testenv.New(t, withFakeDevSessions())
	ctx := context.Background()
	org := env.NewOrg("alice")
	resp, err := env.User(org.Member).APIKey.CreateApiKey(ctx, connect.NewRequest(&tidev1.CreateApiKeyRequest{
		OrganizationId: org.ID, Name: "short", ExpiresAt: timestamppb.New(env.Clock.Now().Add(time.Hour))}))
	if err != nil {
		t.Fatal(err)
	}
	if err := listProfiles(env.Bearer(resp.Msg.GetSecret()), org.ID); err != nil {
		t.Fatalf("before expiry: %v", err)
	}
	env.Clock.Advance(time.Hour)
	wantCode(t, "after expiry", listProfiles(env.Bearer(resp.Msg.GetSecret()), org.ID), connect.CodeUnauthenticated)
	list, _ := env.User(org.Member).APIKey.ListApiKeys(ctx, connect.NewRequest(&tidev1.ListApiKeysRequest{OrganizationId: org.ID}))
	if s := list.Msg.GetApiKeys()[0].GetState(); s != tidev1.ApiKeyState_API_KEY_STATE_EXPIRED {
		t.Fatalf("state %v", s)
	}
}

func TestCreateApiKeyChecksExpiryAfterLockWait(t *testing.T) {
	for _, lock := range []struct {
		name string
		hold func(*testing.T, *testenv.Env, testenv.Org) func()
	}{
		{name: "membership", hold: func(t *testing.T, env *testenv.Env, org testenv.Org) func() {
			return holdTx(t, env, "SELECT 1 FROM memberships WHERE organization_id = $1 AND user_id = $2 FOR UPDATE", org.ID, org.Member)
		}},
		{name: "organization", hold: func(t *testing.T, env *testenv.Env, org testenv.Org) func() {
			return holdTx(t, env, "SELECT 1 FROM organizations WHERE organization_id = $1 FOR UPDATE", org.ID)
		}},
	} {
		for _, tc := range []struct {
			name    string
			elapsed time.Duration
			accept  bool
		}{
			{name: "before expiry", elapsed: 30 * time.Minute, accept: true},
			{name: "at expiry", elapsed: time.Hour},
			{name: "after expiry", elapsed: time.Hour + time.Second},
		} {
			t.Run(lock.name+"/"+tc.name, func(t *testing.T) {
				env := testenv.New(t)
				ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
				defer cancel()
				org := env.NewOrg("alice")
				member := env.User(org.Member)
				expires := env.Clock.Now().Add(time.Hour)
				release := lock.hold(t, env, org)
				type result struct {
					response *connect.Response[tidev1.CreateApiKeyResponse]
					err      error
				}
				done := make(chan result, 1)
				go func() {
					resp, err := member.APIKey.CreateApiKey(ctx, connect.NewRequest(&tidev1.CreateApiKeyRequest{
						OrganizationId: org.ID, Name: "locked", ExpiresAt: timestamppb.New(expires),
					}))
					done <- result{response: resp, err: err}
				}()
				waitForLockWaiter(t, env)
				env.Clock.Advance(tc.elapsed)
				checkTime := env.Clock.Now()
				release()
				got := <-done

				wantWrites := 0
				if tc.accept {
					if got.err != nil {
						t.Fatal(got.err)
					}
					key := got.response.Msg.GetApiKey()
					if key.GetState() != tidev1.ApiKeyState_API_KEY_STATE_ACTIVE || got.response.Msg.GetSecret() == "" ||
						!key.GetCreatedAt().AsTime().Equal(checkTime) || !key.GetExpiresAt().AsTime().Equal(expires) {
						t.Fatal("expected an active key with a secret, the post-lock creation time, and the requested expiry")
					}
					var storedTime time.Time
					if err := env.DB.Pool.QueryRow(ctx, "SELECT create_time FROM api_keys WHERE api_key_id = $1", key.GetApiKeyId()).Scan(&storedTime); err != nil {
						t.Fatal(err)
					}
					if !storedTime.Equal(checkTime) {
						t.Errorf("stored creation time = %v, want %v", storedTime, checkTime)
					}
					wantWrites = 1
				} else {
					wantCode(t, "expiry reached during lock wait", got.err, connect.CodeInvalidArgument)
					if got.err.Error() != "invalid_argument: invalid expires_at: must be in the future" || got.response != nil {
						t.Errorf("error = %v, response present = %v", got.err, got.response != nil)
					}
				}

				var keys int
				if err := env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM api_keys WHERE organization_id = $1 AND user_id = $2", org.ID, org.Member).Scan(&keys); err != nil {
					t.Fatal(err)
				}
				if keys != wantWrites {
					t.Errorf("key rows = %d, want %d", keys, wantWrites)
				}
				if events := countAction(auditActions(t, env, org.ID), tidev1.AuditAction_AUDIT_ACTION_API_KEY_CREATED); events != wantWrites {
					t.Errorf("key creation audit events = %d, want %d", events, wantWrites)
				}
			})
		}
	}
}

// TestPersonalAPIKeyFollowsMembership checks that a key acts with its
// user's current membership: a demoted or removed user's key has no more
// access than the user.
func TestPersonalAPIKeyFollowsMembership(t *testing.T) {
	env := testenv.New(t, withFakeDevSessions())
	ctx := context.Background()
	org := env.NewOrg("alice")
	keyID, secret := env.NewAPIKey(org.ID, org.Admin)
	if err := listProfiles(env.Bearer(secret), org.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.User(org.Owner).Member.RemoveMember(ctx, connect.NewRequest(&tidev1.RemoveMemberRequest{OrganizationId: org.ID, UserId: org.Admin})); err != nil {
		t.Fatal(err)
	}
	wantCode(t, "key after removal", listProfiles(env.Bearer(secret), org.ID), connect.CodeUnauthenticated)
	var revoked bool
	if err := env.DB.Pool.QueryRow(ctx, "SELECT revoke_time IS NOT NULL FROM api_keys WHERE api_key_id = $1", keyID).Scan(&revoked); err != nil || !revoked {
		t.Fatalf("revoked %v, %v", revoked, err)
	}
	// A key whose membership row is gone without a revocation (defense in
	// depth) gets not_found.
	_, secret2 := env.NewAPIKey(org.ID, org.Member)
	if _, err := env.DB.Pool.Exec(ctx, "DELETE FROM memberships WHERE organization_id = $1 AND user_id = $2", org.ID, org.Member); err != nil {
		t.Fatal(err)
	}
	wantCode(t, "key without membership", listProfiles(env.Bearer(secret2), org.ID), connect.CodeNotFound)
}

// A key creation that passed Authorize before a membership removal committed
// must not leave an unrevoked key behind.
func TestCreateApiKeyAfterConcurrentRemoval(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	// The held deletion stands in for a RemoveMember that commits while the
	// creation runs.
	commit := holdTx(t, env, "DELETE FROM memberships WHERE organization_id = $1 AND user_id = $2", org.ID, org.Member)
	errc := make(chan error, 1)
	go func() {
		_, err := env.User(org.Member).APIKey.CreateApiKey(ctx, connect.NewRequest(&tidev1.CreateApiKeyRequest{OrganizationId: org.ID, Name: "late"}))
		errc <- err
	}()
	waitForLockWaiter(t, env)
	commit()
	wantCode(t, "create after removal", <-errc, connect.CodeNotFound)
	var keys int
	if err := env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM api_keys WHERE organization_id = $1 AND user_id = $2", org.ID, org.Member).Scan(&keys); err != nil || keys != 0 {
		t.Fatalf("keys of the removed user = %d, %v", keys, err)
	}
}
