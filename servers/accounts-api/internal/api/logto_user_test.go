// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/servers/accounts-api/internal/billing"
	"github.com/delinoio/mono/servers/accounts-api/internal/directory"
	"github.com/delinoio/mono/servers/accounts-api/internal/email"
	"github.com/delinoio/mono/servers/accounts-api/internal/testenv"
)

// This test uses the HTTP directory client in the API and the billing worker
// so a null profile passes through the same decoder in both flows.
func TestLogtoUserNullProfile(t *testing.T) {
	const ownerEmail = "owner@example.com"
	const inviteeEmail = "invitee@example.com"
	var ownerLookups, inviteeLookups atomic.Int32
	management := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oidc/token":
			_, _ = io.WriteString(w, `{"access_token":"test-token","expires_in":3600}`)
		case "/api/users/owner", "/api/users/invitee":
			count, address := &ownerLookups, ownerEmail
			if r.URL.Path == "/api/users/invitee" {
				count, address = &inviteeLookups, inviteeEmail
			}
			if count.Add(1) == 1 {
				_, _ = io.WriteString(w, "null")
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"primaryEmail": address})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(management.Close)

	lookup := directory.NewLogto(directory.LogtoConfig{
		Endpoint: management.URL, ClientID: "test-client", ClientSecret: "test-secret",
	})
	env := testenv.NewWithDirectory(t, lookup)
	owner := person{ID: "owner", Email: ownerEmail, Client: env.Client(env.Issuer.UserToken(t, "owner"))}
	invitee := person{ID: "invitee", Email: inviteeEmail, Client: env.Client(env.Issuer.UserToken(t, "invitee"))}
	accountID := createAccount(t, owner, "Team").GetBillingAccountId()

	t.Run("invitation remains pending until retry", func(t *testing.T) {
		inv := invite(t, owner, accountID, invitee.Email, roleViewer)
		token := env.InvitationToken(invitee.Email)
		_, err := accept(invitee, token)
		wantCode(t, err, connect.CodeUnavailable)

		list, err := owner.Client.ListInvitations(ctx(), connect.NewRequest(&accountsv1.ListInvitationsRequest{BillingAccountId: accountID}))
		if err != nil {
			t.Fatal(err)
		}
		invitations := list.Msg.GetInvitations()
		if len(invitations) != 1 || invitations[0].GetInvitationId() != inv.GetInvitationId() ||
			invitations[0].GetState() != accountsv1.InvitationState_INVITATION_STATE_PENDING {
			t.Fatalf("invitation did not stay pending: %v", invitations)
		}
		if _, err := accept(invitee, token); err != nil {
			t.Fatalf("retry with the same token: %v", err)
		}
	})

	t.Run("billing recipient remains pending until retry", func(t *testing.T) {
		subscribe(t, env, fixture{Owner: owner, AccountID: accountID})
		org := newOrg(t)
		link(t, env, accountID, org, owner.ID, "Acme")
		env.Clock.Advance(time.Hour + billing.GrantExpireTime(env.Clock.Now()).Sub(env.Clock.Now()))
		w := window(env.Clock.Now()).Add(-billing.WindowLength)
		reportUsage(t, env.Service(), report(org, meterEgress, w, 1, 0))

		if err := env.Billing.DeliverNotifications(ctx()); err != nil {
			t.Fatal(err)
		}
		var attempts, deliveries int
		var settled bool
		err := env.DB.Pool.QueryRow(ctx(), `
			SELECT attempts, send_time IS NOT NULL OR abandon_time IS NOT NULL,
				(SELECT count(*) FROM billing_notification_deliveries d WHERE d.notification_id = n.notification_id)
			FROM billing_notifications n WHERE billing_account_id = $1 AND kind = $2`,
			accountID, string(billing.NotifyLowBalance)).Scan(&attempts, &settled, &deliveries)
		if err != nil || attempts != 1 || settled || deliveries != 0 {
			t.Fatalf("notification after null lookup: attempts=%d settled=%t deliveries=%d err=%v", attempts, settled, deliveries, err)
		}

		env.Clock.Advance(time.Minute)
		if err := env.Billing.DeliverNotifications(ctx()); err != nil {
			t.Fatal(err)
		}
		var sent int
		for _, msg := range env.Email.Messages() {
			if msg.Kind == email.KindLowBalance && msg.To == owner.Email {
				sent++
			}
		}
		if sent != 1 {
			t.Fatalf("sent %d low-balance emails to owner after retry, want 1", sent)
		}
	})
}
