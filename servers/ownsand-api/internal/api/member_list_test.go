// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// tickingClock wraps the clock of the app under test. Until it is armed it
// passes the underlying time through. Once armed, every reading returns a
// time one tick later than the previous reading, which models a clock that
// keeps running while a handler reads its page.
type tickingClock struct {
	mu    sync.Mutex
	next  func() time.Time
	armed bool
	now   time.Time
	tick  time.Duration
}

func (c *tickingClock) read() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.armed {
		return c.next()
	}
	c.now = c.now.Add(c.tick)
	return c.now
}

func (c *tickingClock) arm(start time.Time, tick time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.armed, c.now, c.tick = true, start, tick
}

// insertInvitations writes n invitations of an organization directly, with
// expire_time = base + i*step for i in 1..n. It returns their IDs in
// ascending order, which is the order of ListInvitations.
func insertInvitations(t *testing.T, env *testenv.Env, orgID string, n int, base time.Time, step time.Duration) []string {
	t.Helper()
	ids := make([]string, n)
	for i := range ids {
		id, err := uuidv7.NewString()
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}
	sort.Strings(ids)
	_, err := env.DB.Pool.Exec(context.Background(), `INSERT INTO invitations
		(invitation_id, organization_id, role, secret_sha256, secret_prefix, created_by_kind, created_by_id, create_time, expire_time)
		SELECT id::uuid, $1, $2, sha256(convert_to(id, 'UTF8')), 'osi_test', 1, 'alice', $3, $4::timestamptz + n * $5 * interval '1 microsecond'
		FROM unnest($6::text[]) WITH ORDINALITY AS u(id, n)`,
		orgID, int16(ownsandv1.Role_ROLE_MEMBER), base, base, step.Microseconds(), ids)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func listInvitations(t *testing.T, owner *testenv.Clients, orgID, token string, size int32) *ownsandv1.ListInvitationsResponse {
	t.Helper()
	resp, err := owner.Member.ListInvitations(context.Background(), connect.NewRequest(&ownsandv1.ListInvitationsRequest{
		OrganizationId: orgID, PageSize: size, PageToken: token}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg
}

// TestListInvitationsStateUsesQueryCutoff checks that the clock running past
// an expiry while a page is read does not turn a selected invitation into an
// EXPIRED one: the state of every row comes from the cutoff of the query.
func TestListInvitationsStateUsesQueryCutoff(t *testing.T) {
	clock := &tickingClock{}
	env := testenv.New(t, func(o *app.Options) {
		clock.next = o.Now
		o.Now = clock.read
	})
	org := env.NewOrg("alice")
	// 100 invitations expire one millisecond apart, starting one tick ahead.
	base := env.Clock.Now()
	const n = 100
	insertInvitations(t, env, org.ID, n, base, time.Millisecond)
	clock.arm(base, time.Millisecond)

	page := listInvitations(t, org.OwnerClient, org.ID, "", 100)
	if len(page.GetInvitations()) == 0 {
		t.Fatal("no invitation was listed; the cutoff passed every expiry")
	}
	for _, inv := range page.GetInvitations() {
		if inv.GetState() != ownsandv1.InvitationState_INVITATION_STATE_PENDING {
			t.Fatalf("invitation %s expiring at %s has state %v, want PENDING", inv.GetInvitationId(), inv.GetExpireTime().AsTime(), inv.GetState())
		}
	}
}

// TestListInvitationsExpiryBoundary checks that an invitation is listed only
// while its expiry is after the cutoff.
func TestListInvitationsExpiryBoundary(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	now := env.Clock.Now()
	// Expires exactly at the cutoff, one millisecond before it and one after.
	for _, d := range []time.Duration{0, -time.Millisecond, time.Millisecond} {
		insertInvitations(t, env, org.ID, 1, now.Add(d), 0)
	}
	page := listInvitations(t, org.OwnerClient, org.ID, "", 0)
	if len(page.GetInvitations()) != 1 {
		t.Fatalf("listed %d invitations at the boundary, want 1", len(page.GetInvitations()))
	}
	inv := page.GetInvitations()[0]
	if !inv.GetExpireTime().AsTime().Equal(now.Add(time.Millisecond)) || inv.GetState() != ownsandv1.InvitationState_INVITATION_STATE_PENDING {
		t.Fatalf("listed invitation expires at %s with state %v", inv.GetExpireTime().AsTime(), inv.GetState())
	}
	env.Clock.Advance(time.Millisecond)
	if page := listInvitations(t, org.OwnerClient, org.ID, "", 0); len(page.GetInvitations()) != 0 {
		t.Fatalf("listed %d invitations once the cutoff reached the last expiry", len(page.GetInvitations()))
	}
}

// TestListInvitationsPaginationExcludesNonPending checks the page order and
// the exclusion of accepted, revoked and expired invitations across pages.
func TestListInvitationsPaginationExcludesNonPending(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	ctx := context.Background()
	now := env.Clock.Now()
	var ids []string
	for range 5 {
		ids = append(ids, insertInvitations(t, env, org.ID, 1, now.Add(time.Hour), 0)...)
	}
	sort.Strings(ids)
	// Mark one invitation of each excluded kind: accepted, revoked, expired.
	for _, q := range []string{
		"UPDATE invitations SET accept_time = $2, accepted_by = 'carol' WHERE invitation_id = $1",
		"UPDATE invitations SET revoke_time = $2 WHERE invitation_id = $1",
		"UPDATE invitations SET expire_time = $2 WHERE invitation_id = $1",
	} {
		id := ids[0]
		ids = ids[1:]
		if _, err := env.DB.Pool.Exec(ctx, q, id, now); err != nil {
			t.Fatal(err)
		}
	}
	want := ids

	var got []string
	token := ""
	for pages := 0; ; pages++ {
		if pages > 3 {
			t.Fatal("pagination does not terminate")
		}
		page := listInvitations(t, org.OwnerClient, org.ID, token, 1)
		for _, inv := range page.GetInvitations() {
			if inv.GetState() != ownsandv1.InvitationState_INVITATION_STATE_PENDING {
				t.Fatalf("invitation %s has state %v", inv.GetInvitationId(), inv.GetState())
			}
			got = append(got, inv.GetInvitationId())
		}
		token = page.GetNextPageToken()
		if token == "" {
			break
		}
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("listed %v, want %v", got, want)
	}
}
