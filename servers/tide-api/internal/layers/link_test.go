// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/api"
	"github.com/delinoio/mono/servers/tide-api/internal/layers"
	"github.com/delinoio/mono/servers/tide-api/internal/ownsandclient/ownsandfake"
	"github.com/delinoio/mono/servers/tide-api/internal/ratelimit"
	"github.com/delinoio/mono/servers/tide-api/internal/testenv"
)

func TestOwnSandLink(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	osOrg, otherOS := api.NewID(), api.NewID()
	w.fake.AddOrg(osOrg, ownsandKey('a'))
	w.fake.AddOrg(otherOS, ownsandKey('b'))

	got, err := w.env.User(w.org.Member).OwnSandLink.GetOwnSandLink(ctx, connect.NewRequest(&tidev1.GetOwnSandLinkRequest{OrganizationId: w.org.ID}))
	if err != nil || got.Msg.GetLink() != nil {
		t.Fatalf("no link yet: %v %v", got, err)
	}
	_, err = w.setLink(w.org.Member, osOrg, ownsandKey('a'))
	wantCode(t, "member sets", err, connect.CodePermissionDenied)
	_, err = w.setLink(w.org.Admin, osOrg, "not-a-key")
	wantCode(t, "malformed key", err, connect.CodeInvalidArgument)
	_, err = w.setLink(w.org.Admin, osOrg, ownsandKey('z'))
	wantCode(t, "unknown key", err, connect.CodeInvalidArgument)
	_, err = w.setLink(w.org.Admin, osOrg, ownsandKey('b'))
	wantCode(t, "key of another OwnSand organization", err, connect.CodeInvalidArgument)

	link, err := w.setLink(w.org.Admin, osOrg, ownsandKey('a'))
	if err != nil || link.GetState() != tidev1.OwnSandLinkState_OWN_SAND_LINK_STATE_ACTIVE || !link.GetCredentialSet() || link.GetCredentialSetAt() == nil ||
		strings.Contains(link.String(), ownsandKey('a')) {
		t.Fatalf("link %v, %v", link, err)
	}
	// The key is stored encrypted.
	var ciphertext []byte
	if err := w.env.DB.Pool.QueryRow(ctx, "SELECT api_key_ciphertext FROM ownsand_links WHERE organization_id = $1", w.org.ID).Scan(&ciphertext); err != nil ||
		bytes.Contains(ciphertext, []byte(ownsandKey('a'))) {
		t.Fatalf("stored key: %v", err)
	}
	// Another tide organization cannot link the same OwnSand organization.
	other := w.env.NewOrg("bob")
	_, err = w.env.User("bob").OwnSandLink.SetOwnSandLink(ctx, connect.NewRequest(&tidev1.SetOwnSandLinkRequest{
		OrganizationId: other.ID, OwnsandOrganizationId: osOrg, OwnsandApiKey: ownsandKey('a')}))
	wantCode(t, "second tide organization", err, connect.CodeAlreadyExists)
	// Another OwnSand organization needs RemoveOwnSandLink first.
	_, err = w.setLink(w.org.Owner, otherOS, ownsandKey('b'))
	wantCode(t, "change of OwnSand organization", err, connect.CodeFailedPrecondition)

	// OwnSand rejects the key: the link becomes INVALID.
	w.fake.RevokeKey(osOrg)
	client, _, err := w.l.Links.Client(ctx, w.org.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListDevices(ctx); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("revoked key: %v", err)
	}
	got, _ = w.env.User(w.org.Member).OwnSandLink.GetOwnSandLink(ctx, connect.NewRequest(&tidev1.GetOwnSandLinkRequest{OrganizationId: w.org.ID}))
	if got.Msg.GetLink().GetState() != tidev1.OwnSandLinkState_OWN_SAND_LINK_STATE_INVALID {
		t.Fatalf("state after unauthenticated: %v", got.Msg.GetLink())
	}
	if _, _, err := w.l.Links.Client(ctx, w.org.ID); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("client of an invalid link: %v", err)
	}
	// A new key replaces the old one and makes the link ACTIVE again.
	w.fake.AddOrg(osOrg, ownsandKey('c'))
	w.env.Clock.Advance(time.Second)
	link2, err := w.setLink(w.org.Admin, osOrg, ownsandKey('c'))
	if err != nil || link2.GetState() != tidev1.OwnSandLinkState_OWN_SAND_LINK_STATE_ACTIVE || !link2.GetCredentialSetAt().AsTime().After(link.GetCredentialSetAt().AsTime()) {
		t.Fatalf("replace: %v %v", link2, err)
	}
	client, _, err = w.l.Links.Client(ctx, w.org.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListDevices(ctx); err != nil {
		t.Fatalf("new key: %v", err)
	}

	// Personal API keys are rejected.
	_, secret := w.env.NewAPIKey(w.org.ID, w.org.Owner)
	_, err = w.env.Bearer(secret).OwnSandLink.GetOwnSandLink(ctx, connect.NewRequest(&tidev1.GetOwnSandLinkRequest{OrganizationId: w.org.ID}))
	wantCode(t, "personal API key", err, connect.CodePermissionDenied)

	if _, err := w.env.User(w.org.Admin).OwnSandLink.RemoveOwnSandLink(ctx, connect.NewRequest(&tidev1.RemoveOwnSandLinkRequest{OrganizationId: w.org.ID})); err != nil {
		t.Fatal(err)
	}
	_, err = w.env.User(w.org.Admin).OwnSandLink.RemoveOwnSandLink(ctx, connect.NewRequest(&tidev1.RemoveOwnSandLinkRequest{OrganizationId: w.org.ID}))
	wantCode(t, "remove twice", err, connect.CodeNotFound)
	if countAudit(t, w.env, w.org.ID, tidev1.AuditAction_AUDIT_ACTION_OWNSAND_LINK_SET) != 2 || countAudit(t, w.env, w.org.ID, tidev1.AuditAction_AUDIT_ACTION_OWNSAND_LINK_REMOVED) != 1 {
		t.Fatal("audit events")
	}
	// After the removal, any OwnSand organization can be linked.
	if _, err := w.setLink(w.org.Owner, otherOS, ownsandKey('b')); err != nil {
		t.Fatal(err)
	}
}

func TestOwnSandLinkValidationRateLimit(t *testing.T) {
	var env *testenv.Env
	now := func() time.Time { return env.Clock.Now() }
	limits := testenv.GenerousLimits(now)
	limits.PerUser = ratelimit.New(10.0/60, 10, now)
	w := &world{fake: ownsandfake.New(), l: &layers.Layers{}}
	env = testenv.New(t, testenv.WithLayers(w.fake, w.l), testenv.WithLimits(limits))
	w.env = env
	w.org = env.NewOrg("alice")
	osOrg := api.NewID()
	w.fake.AddOrg(osOrg, ownsandKey('a'))
	for i := 0; i < 10; i++ {
		_, err := w.setLink(w.org.Admin, osOrg, ownsandKey('z'))
		wantCode(t, "wrong key", err, connect.CodeInvalidArgument)
	}
	_, err := w.setLink(w.org.Admin, osOrg, ownsandKey('a'))
	wantCode(t, "11th validation", err, connect.CodeResourceExhausted)
	if n := w.fake.Calls("ListDevices"); n != 10 {
		t.Fatalf("%d validations reached OwnSand", n)
	}
}

// The production clock has sub-microsecond precision, and PostgreSQL stores
// microseconds; setting and replacing the key must still succeed.
func TestOwnSandLinkWithSubMicrosecondClock(t *testing.T) {
	w := newWorld(t)
	osOrg := api.NewID()
	w.fake.AddOrg(osOrg, ownsandKey('a'))
	w.env.Clock.Advance(1500 * time.Nanosecond)
	if _, err := w.setLink(w.org.Admin, osOrg, ownsandKey('a')); err != nil {
		t.Fatalf("set: %v", err)
	}
	w.fake.AddOrg(osOrg, ownsandKey('b'))
	w.env.Clock.Advance(time.Second + 700*time.Nanosecond)
	if _, err := w.setLink(w.org.Admin, osOrg, ownsandKey('b')); err != nil {
		t.Fatalf("replace: %v", err)
	}
}
