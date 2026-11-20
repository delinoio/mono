// SPDX-License-Identifier: FSL-1.1-ALv2

package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/ratelimit"
	"github.com/delinoio/mono/servers/ownsand-api/internal/secret"
)

type fakeStore struct {
	keys     map[string]APIKeyRecord
	sessions map[string]DeviceSessionRecord
}

func (f *fakeStore) LookupAPIKey(_ context.Context, h []byte) (APIKeyRecord, error) {
	r, ok := f.keys[string(h)]
	if !ok {
		return r, ErrNotFound
	}
	return r, nil
}

func (f *fakeStore) LookupDeviceSession(_ context.Context, h []byte) (DeviceSessionRecord, error) {
	r, ok := f.sessions[string(h)]
	if !ok {
		return r, ErrNotFound
	}
	return r, nil
}

type countingTokens struct{ calls int }

func (c *countingTokens) Validate(context.Context, string) (UserClaims, error) {
	c.calls++
	return UserClaims{Subject: "user-1"}, nil
}

func TestAuthenticatorSelectsOneValidator(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	key := secret.New(secret.KindAPIKey)
	revoked := secret.New(secret.KindAPIKey)
	dev := secret.New(secret.KindDeviceSession)
	expired := secret.New(secret.KindDeviceSession)
	removed := secret.New(secret.KindDeviceSession)
	st := &fakeStore{
		keys: map[string]APIKeyRecord{
			string(secret.Hash(key)):     {APIKeyID: "k1", OrganizationID: "o1", SecretHash: secret.Hash(key)},
			string(secret.Hash(revoked)): {APIKeyID: "k2", OrganizationID: "o1", SecretHash: secret.Hash(revoked), Revoked: true},
		},
		sessions: map[string]DeviceSessionRecord{
			string(secret.Hash(dev)):     {DeviceID: "d1", OrganizationID: "o1", CredentialHash: secret.Hash(dev), ExpiresAt: now.Add(time.Minute)},
			string(secret.Hash(expired)): {DeviceID: "d2", OrganizationID: "o1", CredentialHash: secret.Hash(expired), ExpiresAt: now},
			string(secret.Hash(removed)): {DeviceID: "d3", OrganizationID: "o1", CredentialHash: secret.Hash(removed), ExpiresAt: now.Add(time.Minute), DeviceRemoved: true},
		},
	}
	tokens := &countingTokens{}
	a := &Authenticator{Store: st, Tokens: tokens, Now: func() time.Time { return now }}
	ctx := context.Background()

	p, err := a.Authenticate(ctx, key)
	if err != nil || p.Kind != ownsandv1.PrincipalKind_PRINCIPAL_KIND_API_KEY || p.ID != "k1" || p.OrganizationID != "o1" {
		t.Fatalf("api key: %+v %v", p, err)
	}
	p, err = a.Authenticate(ctx, dev)
	if err != nil || p.Kind != ownsandv1.PrincipalKind_PRINCIPAL_KIND_DEVICE || p.ID != "d1" {
		t.Fatalf("device: %+v %v", p, err)
	}
	p, err = a.Authenticate(ctx, "a.b.c")
	if err != nil || p.Kind != ownsandv1.PrincipalKind_PRINCIPAL_KIND_USER || p.ID != "user-1" {
		t.Fatalf("user: %+v %v", p, err)
	}
	for name, cred := range map[string]string{
		"revoked key": revoked, "unknown key": secret.New(secret.KindAPIKey), "expired device": expired,
		"removed device": removed, "enrollment token": secret.New(secret.KindEnrollmentToken),
		"invitation": secret.New(secret.KindInvitation), "malformed key with dots": "osk_a.b.c", "garbage": "nope",
	} {
		before := tokens.calls
		_, err := a.Authenticate(ctx, cred)
		if connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Errorf("%s: got %v", name, err)
		}
		if tokens.calls != before {
			t.Errorf("%s: the JWT validator ran after another validator rejected the credential", name)
		}
	}
}

func TestAPIKeyFailureRateLimit(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a := &Authenticator{Store: &fakeStore{}, Failures: ratelimit.New(1.0/6, 3, func() time.Time { return now }), Now: func() time.Time { return now }}
	cred := secret.New(secret.KindAPIKey)
	for i := 0; i < 3; i++ {
		if _, err := a.Authenticate(context.Background(), cred); connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	_, err := a.Authenticate(context.Background(), cred)
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("after the burst: %v", err)
	}
	if errors.Is(err, ErrInvalidCredential) {
		t.Fatal("rate limit error must differ from the invalid credential error")
	}
}

func TestBearerToken(t *testing.T) {
	if v, ok := BearerToken("Bearer abc"); !ok || v != "abc" {
		t.Fatal("bearer")
	}
	if v, ok := BearerToken("bearer   abc "); !ok || v != "abc" {
		t.Fatal("case-insensitive scheme")
	}
	for _, h := range []string{"", "Basic abc", "Bearer", "Bearer "} {
		if _, ok := BearerToken(h); ok {
			t.Errorf("%q accepted", h)
		}
	}
}
