// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"testing"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
	"github.com/delinoio/mono/servers/ownsand-api/internal/events"
	"github.com/delinoio/mono/servers/ownsand-api/internal/grants"
	"github.com/delinoio/mono/servers/ownsand-api/internal/hub"
	"github.com/delinoio/mono/servers/ownsand-api/internal/relay"
)

func TestMembershipRevocationCursor(t *testing.T) {
	e := events.Event{Kind: events.KindMembershipEnded, ID: 2, OrganizationID: "org", UserID: "user"}
	for _, tc := range []struct {
		name       string
		org, user  string
		deliveryID int64
		revoked    bool
	}{
		{name: "older", org: "org", user: "user", deliveryID: 1, revoked: true},
		{name: "equal", org: "org", user: "user", deliveryID: 2},
		{name: "newer", org: "org", user: "user", deliveryID: 3},
		{name: "different organization", org: "other", user: "user", deliveryID: 1},
		{name: "different user", org: "org", user: "other", deliveryID: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			p := NewDataPlane(&Service{Hub: hub.New(), Now: func() time.Time { return now }}, relay.Limits{}, RelayTiming{})
			pr := auth.Principal{Kind: ownsandv1.PrincipalKind_PRINCIPAL_KIND_USER, ID: tc.user}
			membership := auth.MembershipSnapshot{ID: "membership", DeliveryID: tc.deliveryID}
			p.sessions["session"] = &session{id: "session", orgID: tc.org, principal: pr, membership: membership, expireAt: now.Add(time.Hour)}
			for _, id := range []string{"pending", "installed"} {
				if _, _, err := p.Grants.Reserve(grants.Request{AuthorizationID: id, OrgID: tc.org, Principal: pr, Membership: membership}, func() string { return id }); err != nil {
					t.Fatal(err)
				}
			}
			p.Grants.Ack("installed", ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_ACCEPTED, "")

			p.HandleEvent(t.Context(), e)

			if _, exists := p.sessions["session"]; exists == tc.revoked {
				t.Errorf("session exists = %t, want %t", exists, !tc.revoked)
			}
			for _, id := range []string{"pending", "installed"} {
				if exists := p.Grants.Get(id) != nil; exists == tc.revoked {
					t.Errorf("%s grant exists = %t, want %t", id, exists, !tc.revoked)
				}
			}
		})
	}
}
