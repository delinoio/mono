// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/protos/delino/accounts/v1/accountsv1connect"
	"github.com/delinoio/mono/servers/accounts-api/internal/api"
	"github.com/delinoio/mono/servers/accounts-api/internal/auth"
	"github.com/delinoio/mono/servers/accounts-api/internal/auth/authtest"
	"github.com/delinoio/mono/servers/accounts-api/internal/email"
	"github.com/delinoio/mono/servers/accounts-api/internal/testenv"
)

const (
	roleOwner  = accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_OWNER
	roleAdmin  = accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_ADMIN
	roleViewer = accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_VIEWER
)

var userSeq atomic.Int64

// person is a test user with a client.
type person struct {
	ID     string
	Email  string
	Client accountsv1connect.BillingAccountServiceClient
}

func newPerson(env *testenv.Env, label string) person {
	n := userSeq.Add(1)
	id := fmt.Sprintf("%s-%d", label, n)
	addr := fmt.Sprintf("%s-%d@example.com", label, n)
	return person{ID: id, Email: addr, Client: env.User(id, addr, "Name of "+id)}
}

func ctx() context.Context { return context.Background() }

func wantCode(t *testing.T, err error, want connect.Code) {
	t.Helper()
	if want == 0 {
		if err != nil {
			t.Fatalf("error = %v, want success", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("error = nil, want %s", want)
	}
	if got := connect.CodeOf(err); got != want {
		t.Fatalf("code = %s (%v), want %s", got, err, want)
	}
}

func createAccount(t *testing.T, p person, name string) *accountsv1.BillingAccount {
	t.Helper()
	resp, err := p.Client.CreateBillingAccount(ctx(), connect.NewRequest(&accountsv1.CreateBillingAccountRequest{Name: name}))
	if err != nil {
		t.Fatalf("CreateBillingAccount: %v", err)
	}
	return resp.Msg.GetBillingAccount()
}

func invite(t *testing.T, inviter person, accountID, addr string, role accountsv1.BillingAccountRole) *accountsv1.BillingAccountInvitation {
	t.Helper()
	resp, err := inviter.Client.CreateInvitation(ctx(), connect.NewRequest(&accountsv1.CreateInvitationRequest{
		BillingAccountId: accountID, Email: addr, Role: role,
	}))
	if err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}
	return resp.Msg.GetInvitation()
}

func accept(p person, token string) (*connect.Response[accountsv1.AcceptInvitationResponse], error) {
	return p.Client.AcceptInvitation(ctx(), connect.NewRequest(&accountsv1.AcceptInvitationRequest{InvitationToken: token}))
}

// join invites p with the role and accepts the invitation.
func join(t *testing.T, env *testenv.Env, inviter person, accountID string, p person, role accountsv1.BillingAccountRole) {
	t.Helper()
	invite(t, inviter, accountID, p.Email, role)
	if _, err := accept(p, env.InvitationToken(p.Email)); err != nil {
		t.Fatalf("AcceptInvitation: %v", err)
	}
}

// fixture is a billing account with an owner, an admin, two viewers, an
// outsider and one pending invitation.
type fixture struct {
	AccountID    string
	Owner        person
	Admin        person
	Viewer       person
	Viewer2      person
	Outsider     person
	InvitationID string
}

func newFixture(t *testing.T, env *testenv.Env) fixture {
	t.Helper()
	f := fixture{
		Owner: newPerson(env, "owner"), Admin: newPerson(env, "admin"), Viewer: newPerson(env, "viewer"),
		Viewer2: newPerson(env, "viewer2"), Outsider: newPerson(env, "outsider"),
	}
	f.AccountID = createAccount(t, f.Owner, "Fixture").GetBillingAccountId()
	join(t, env, f.Owner, f.AccountID, f.Admin, roleAdmin)
	join(t, env, f.Owner, f.AccountID, f.Viewer, roleViewer)
	join(t, env, f.Admin, f.AccountID, f.Viewer2, roleViewer)
	f.InvitationID = invite(t, f.Owner, f.AccountID, fmt.Sprintf("pending-%d@example.com", userSeq.Add(1)), roleViewer).GetInvitationId()
	return f
}

func (f fixture) person(role string) person {
	switch role {
	case "owner":
		return f.Owner
	case "admin":
		return f.Admin
	case "viewer":
		return f.Viewer
	default:
		return f.Outsider
	}
}

func TestRoleMatrix(t *testing.T) {
	env := testenv.New(t)
	const (
		ok   = connect.Code(0)
		perm = connect.CodePermissionDenied
		nf   = connect.CodeNotFound
		pre  = connect.CodeFailedPrecondition
	)
	type op func(f fixture, c accountsv1connect.BillingAccountServiceClient) error
	ops := []struct {
		name string
		run  op
		// want is the result for owner, admin, viewer and outsider.
		want [4]connect.Code
	}{
		{"GetBillingAccount", func(f fixture, c accountsv1connect.BillingAccountServiceClient) error {
			_, err := c.GetBillingAccount(ctx(), connect.NewRequest(&accountsv1.GetBillingAccountRequest{BillingAccountId: f.AccountID}))
			return err
		}, [4]connect.Code{ok, ok, ok, nf}},
		{"UpdateBillingAccount", func(f fixture, c accountsv1connect.BillingAccountServiceClient) error {
			_, err := c.UpdateBillingAccount(ctx(), connect.NewRequest(&accountsv1.UpdateBillingAccountRequest{BillingAccountId: f.AccountID, Name: "Renamed"}))
			return err
		}, [4]connect.Code{ok, ok, perm, nf}},
		{"DeleteBillingAccount", func(f fixture, c accountsv1connect.BillingAccountServiceClient) error {
			_, err := c.DeleteBillingAccount(ctx(), connect.NewRequest(&accountsv1.DeleteBillingAccountRequest{BillingAccountId: f.AccountID}))
			return err
		}, [4]connect.Code{ok, perm, perm, nf}},
		{"CreateInvitation", func(f fixture, c accountsv1connect.BillingAccountServiceClient) error {
			_, err := c.CreateInvitation(ctx(), connect.NewRequest(&accountsv1.CreateInvitationRequest{BillingAccountId: f.AccountID, Email: "new@example.com", Role: roleViewer}))
			return err
		}, [4]connect.Code{ok, ok, perm, nf}},
		{"ListInvitations", func(f fixture, c accountsv1connect.BillingAccountServiceClient) error {
			_, err := c.ListInvitations(ctx(), connect.NewRequest(&accountsv1.ListInvitationsRequest{BillingAccountId: f.AccountID}))
			return err
		}, [4]connect.Code{ok, ok, perm, nf}},
		{"RevokeInvitation", func(f fixture, c accountsv1connect.BillingAccountServiceClient) error {
			_, err := c.RevokeInvitation(ctx(), connect.NewRequest(&accountsv1.RevokeInvitationRequest{BillingAccountId: f.AccountID, InvitationId: f.InvitationID}))
			return err
		}, [4]connect.Code{ok, ok, perm, nf}},
		{"ListMembers", func(f fixture, c accountsv1connect.BillingAccountServiceClient) error {
			_, err := c.ListMembers(ctx(), connect.NewRequest(&accountsv1.ListMembersRequest{BillingAccountId: f.AccountID}))
			return err
		}, [4]connect.Code{ok, ok, ok, nf}},
		{"UpdateMemberRole", func(f fixture, c accountsv1connect.BillingAccountServiceClient) error {
			_, err := c.UpdateMemberRole(ctx(), connect.NewRequest(&accountsv1.UpdateMemberRoleRequest{BillingAccountId: f.AccountID, UserId: f.Viewer2.ID, Role: roleAdmin}))
			return err
		}, [4]connect.Code{ok, ok, perm, nf}},
		{"RemoveMember other", func(f fixture, c accountsv1connect.BillingAccountServiceClient) error {
			_, err := c.RemoveMember(ctx(), connect.NewRequest(&accountsv1.RemoveMemberRequest{BillingAccountId: f.AccountID, UserId: f.Viewer2.ID}))
			return err
		}, [4]connect.Code{ok, ok, perm, nf}},
		{"RemoveMember admin", func(f fixture, c accountsv1connect.BillingAccountServiceClient) error {
			_, err := c.RemoveMember(ctx(), connect.NewRequest(&accountsv1.RemoveMemberRequest{BillingAccountId: f.AccountID, UserId: f.Admin.ID}))
			return err
		}, [4]connect.Code{ok, ok, perm, nf}},
		{"RemoveMember owner", func(f fixture, c accountsv1connect.BillingAccountServiceClient) error {
			_, err := c.RemoveMember(ctx(), connect.NewRequest(&accountsv1.RemoveMemberRequest{BillingAccountId: f.AccountID, UserId: f.Owner.ID}))
			return err
		}, [4]connect.Code{pre, pre, perm, nf}},
		{"TransferOwnership", func(f fixture, c accountsv1connect.BillingAccountServiceClient) error {
			_, err := c.TransferOwnership(ctx(), connect.NewRequest(&accountsv1.TransferOwnershipRequest{BillingAccountId: f.AccountID, UserId: f.Viewer2.ID}))
			return err
		}, [4]connect.Code{ok, perm, perm, nf}},
	}
	roles := []string{"owner", "admin", "viewer", "outsider"}
	for _, o := range ops {
		for i, role := range roles {
			t.Run(o.name+"/"+role, func(t *testing.T) {
				f := newFixture(t, env)
				wantCode(t, o.run(f, f.person(role).Client), o.want[i])
			})
		}
	}
	// Self-removal: the owner cannot leave; admins and viewers can.
	for role, want := range map[string]connect.Code{"owner": pre, "admin": ok, "viewer": ok, "outsider": nf} {
		t.Run("RemoveMember self/"+role, func(t *testing.T) {
			f := newFixture(t, env)
			p := f.person(role)
			_, err := p.Client.RemoveMember(ctx(), connect.NewRequest(&accountsv1.RemoveMemberRequest{BillingAccountId: f.AccountID, UserId: p.ID}))
			wantCode(t, err, want)
			if want == ok {
				_, err := p.Client.GetBillingAccount(ctx(), connect.NewRequest(&accountsv1.GetBillingAccountRequest{BillingAccountId: f.AccountID}))
				wantCode(t, err, nf)
			}
		})
	}
}

func TestCreateGetListBillingAccounts(t *testing.T) {
	env := testenv.New(t)
	alice := newPerson(env, "alice")
	bob := newPerson(env, "bob")

	created := createAccount(t, alice, "  Alice's account \t")
	if created.GetName() != "Alice's account" || created.GetCallerRole() != roleOwner || created.GetCreateTime() == nil {
		t.Fatalf("created = %v", created)
	}
	id := created.GetBillingAccountId()
	if id != strings.ToLower(id) {
		t.Fatalf("billing_account_id %q is not lowercase", id)
	}

	got, err := alice.Client.GetBillingAccount(ctx(), connect.NewRequest(&accountsv1.GetBillingAccountRequest{BillingAccountId: strings.ToUpper(id)}))
	if err != nil {
		t.Fatalf("GetBillingAccount with an uppercase ID: %v", err)
	}
	if got.Msg.GetBillingAccount().GetBillingAccountId() != id || !got.Msg.GetBillingAccount().GetCreateTime().AsTime().Equal(created.GetCreateTime().AsTime()) {
		t.Fatalf("got = %v", got.Msg.GetBillingAccount())
	}

	_, err = alice.Client.GetBillingAccount(ctx(), connect.NewRequest(&accountsv1.GetBillingAccountRequest{BillingAccountId: "not-a-uuid"}))
	wantCode(t, err, connect.CodeInvalidArgument)
	_, err = alice.Client.GetBillingAccount(ctx(), connect.NewRequest(&accountsv1.GetBillingAccountRequest{BillingAccountId: "01900000-0000-4000-8000-000000000000"}))
	wantCode(t, err, connect.CodeInvalidArgument)
	_, err = alice.Client.GetBillingAccount(ctx(), connect.NewRequest(&accountsv1.GetBillingAccountRequest{BillingAccountId: "01900000-0000-7000-8000-000000000000"}))
	wantCode(t, err, connect.CodeNotFound)
	_, err = bob.Client.GetBillingAccount(ctx(), connect.NewRequest(&accountsv1.GetBillingAccountRequest{BillingAccountId: id}))
	wantCode(t, err, connect.CodeNotFound)

	for _, name := range []string{"", "   ", strings.Repeat("a", 101), "bad\x07name", "line\nbreak"} {
		_, err := alice.Client.CreateBillingAccount(ctx(), connect.NewRequest(&accountsv1.CreateBillingAccountRequest{Name: name}))
		wantCode(t, err, connect.CodeInvalidArgument)
	}
	// 100 code points of a multi-byte character are valid.
	createAccount(t, bob, strings.Repeat("가", 100))

	ids := []string{id}
	for i := range 4 {
		ids = append(ids, createAccount(t, alice, fmt.Sprintf("Account %d", i)).GetBillingAccountId())
	}
	// Pages of 2 return every account in creation order.
	var listed []string
	token := ""
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("pagination does not end")
		}
		resp, err := alice.Client.ListBillingAccounts(ctx(), connect.NewRequest(&accountsv1.ListBillingAccountsRequest{PageSize: 2, PageToken: token}))
		if err != nil {
			t.Fatalf("ListBillingAccounts: %v", err)
		}
		for _, a := range resp.Msg.GetBillingAccounts() {
			if a.GetCallerRole() != roleOwner {
				t.Fatalf("caller_role = %v", a.GetCallerRole())
			}
			listed = append(listed, a.GetBillingAccountId())
		}
		token = resp.Msg.GetNextPageToken()
		if token == "" {
			break
		}
		// Another caller cannot use the token.
		_, err = bob.Client.ListBillingAccounts(ctx(), connect.NewRequest(&accountsv1.ListBillingAccountsRequest{PageToken: token}))
		wantCode(t, err, connect.CodeInvalidArgument)
	}
	if strings.Join(listed, ",") != strings.Join(ids, ",") {
		t.Fatalf("listed %v, want %v", listed, ids)
	}
	_, err = alice.Client.ListBillingAccounts(ctx(), connect.NewRequest(&accountsv1.ListBillingAccountsRequest{PageSize: -1}))
	wantCode(t, err, connect.CodeInvalidArgument)
	_, err = alice.Client.ListBillingAccounts(ctx(), connect.NewRequest(&accountsv1.ListBillingAccountsRequest{PageToken: "garbage"}))
	wantCode(t, err, connect.CodeInvalidArgument)
	resp, err := alice.Client.ListBillingAccounts(ctx(), connect.NewRequest(&accountsv1.ListBillingAccountsRequest{PageSize: 500}))
	if err != nil || len(resp.Msg.GetBillingAccounts()) != 5 || resp.Msg.GetNextPageToken() != "" {
		t.Fatalf("ListBillingAccounts(500) = %v, %v", resp, err)
	}

	renamed, err := alice.Client.UpdateBillingAccount(ctx(), connect.NewRequest(&accountsv1.UpdateBillingAccountRequest{BillingAccountId: id, Name: " New name "}))
	if err != nil || renamed.Msg.GetBillingAccount().GetName() != "New name" {
		t.Fatalf("UpdateBillingAccount = %v, %v", renamed, err)
	}
	_, err = alice.Client.UpdateBillingAccount(ctx(), connect.NewRequest(&accountsv1.UpdateBillingAccountRequest{BillingAccountId: id, Name: ""}))
	wantCode(t, err, connect.CodeInvalidArgument)
}

func TestInvitationAcceptance(t *testing.T) {
	env := testenv.New(t)
	owner := newPerson(env, "owner")
	invitee := newPerson(env, "invitee")
	other := newPerson(env, "other")
	accountID := createAccount(t, owner, "Team").GetBillingAccountId()

	before := len(env.Email.Messages())
	// The inviter enters the address in another letter case.
	inv := invite(t, owner, accountID, "  "+strings.ToUpper(invitee.Email)+" ", roleAdmin)
	if inv.GetState() != accountsv1.InvitationState_INVITATION_STATE_PENDING || inv.GetEmail() != strings.ToUpper(invitee.Email) ||
		inv.GetRole() != roleAdmin || inv.GetInviterUserId() != owner.ID {
		t.Fatalf("invitation = %v", inv)
	}
	if got := inv.GetExpireTime().AsTime().Sub(inv.GetCreateTime().AsTime()); got != 7*24*time.Hour {
		t.Fatalf("lifetime = %v, want 7 days", got)
	}
	msgs := env.Email.Messages()[before:]
	if len(msgs) != 1 {
		t.Fatalf("%d invitation emails, want exactly 1", len(msgs))
	}
	msg := msgs[0]
	if msg.To != strings.ToUpper(invitee.Email) || msg.Kind != "invitation" || msg.IdempotencyKey != "invitation/"+inv.GetInvitationId() {
		t.Fatalf("email = %+v", msg)
	}
	token := env.InvitationToken(strings.ToUpper(invitee.Email))
	if !strings.HasPrefix(token, "bai_") || len(token) != 4+43 {
		t.Fatalf("token %q has the wrong format", token)
	}
	if !strings.Contains(msg.Text, testenv.WebOrigin+"/billing/invitations/accept#token="+token) {
		t.Fatalf("email text misses the link: %s", msg.Text)
	}
	// The response never carries the token.
	if strings.Contains(inv.String(), token) {
		t.Fatal("the CreateInvitation response contains the token")
	}

	// Another signed-in user cannot accept; the invitation stays pending.
	_, err := accept(other, token)
	wantCode(t, err, connect.CodePermissionDenied)
	list, err := owner.Client.ListInvitations(ctx(), connect.NewRequest(&accountsv1.ListInvitationsRequest{BillingAccountId: accountID}))
	if err != nil || len(list.Msg.GetInvitations()) != 1 {
		t.Fatalf("ListInvitations after the rejected accept = %v, %v", list, err)
	}

	// The invited user accepts and gets the invited role.
	resp, err := accept(invitee, token)
	if err != nil {
		t.Fatalf("AcceptInvitation: %v", err)
	}
	if resp.Msg.GetBillingAccount().GetCallerRole() != roleAdmin || resp.Msg.GetMember().GetRole() != roleAdmin ||
		resp.Msg.GetMember().GetUserId() != invitee.ID || resp.Msg.GetMember().GetDisplayName() != "Name of "+invitee.ID {
		t.Fatalf("AcceptInvitation = %v", resp.Msg)
	}
	got, err := invitee.Client.GetBillingAccount(ctx(), connect.NewRequest(&accountsv1.GetBillingAccountRequest{BillingAccountId: accountID}))
	if err != nil || got.Msg.GetBillingAccount().GetCallerRole() != roleAdmin {
		t.Fatalf("GetBillingAccount as the new member = %v, %v", got, err)
	}

	// A reused invitation fails, also for the user who accepted it.
	_, err = accept(invitee, token)
	wantCode(t, err, connect.CodeFailedPrecondition)
	_, err = accept(other, token)
	wantCode(t, err, connect.CodeFailedPrecondition)

	// Unknown and malformed tokens.
	_, err = accept(invitee, "bai_"+strings.Repeat("A", 43))
	wantCode(t, err, connect.CodeNotFound)
	_, err = accept(invitee, "nonsense")
	wantCode(t, err, connect.CodeNotFound)
	_, err = accept(invitee, "")
	wantCode(t, err, connect.CodeInvalidArgument)

	// An existing member gets already_exists, and the invitation stays
	// pending.
	invite(t, owner, accountID, invitee.Email, roleViewer)
	second := env.InvitationToken(invitee.Email)
	_, err = accept(invitee, second)
	wantCode(t, err, connect.CodeAlreadyExists)
	list, err = owner.Client.ListInvitations(ctx(), connect.NewRequest(&accountsv1.ListInvitationsRequest{BillingAccountId: accountID}))
	if err != nil || len(list.Msg.GetInvitations()) != 1 {
		t.Fatalf("ListInvitations after already_exists = %v, %v", list, err)
	}

	// The invited user's Logto account without the primary email cannot
	// accept, and an unreadable profile is unavailable.
	late := newPerson(env, "late")
	invite(t, owner, accountID, late.Email, roleViewer)
	lateToken := env.InvitationToken(late.Email)
	env.Directory.SetErr(errors.New("logto down"))
	_, err = accept(late, lateToken)
	wantCode(t, err, connect.CodeUnavailable)
	env.Directory.SetErr(nil)
	unknown := env.Client(env.Issuer.UserToken(t, "not-in-logto"))
	_, err = unknown.AcceptInvitation(ctx(), connect.NewRequest(&accountsv1.AcceptInvitationRequest{InvitationToken: lateToken}))
	wantCode(t, err, connect.CodePermissionDenied)

	// An expired invitation fails.
	env.Clock.Advance(7*24*time.Hour + time.Second)
	_, err = accept(late, lateToken)
	wantCode(t, err, connect.CodeFailedPrecondition)
	list, err = owner.Client.ListInvitations(ctx(), connect.NewRequest(&accountsv1.ListInvitationsRequest{BillingAccountId: accountID}))
	if err != nil || len(list.Msg.GetInvitations()) != 0 {
		t.Fatalf("ListInvitations after expiry = %v, %v", list, err)
	}
	// After expiry, the same address can be invited again.
	invite(t, owner, accountID, late.Email, roleViewer)
	if _, err := accept(late, env.InvitationToken(late.Email)); err != nil {
		t.Fatalf("AcceptInvitation of the new invitation: %v", err)
	}
}

// TestInvitationTokenHelperIgnoresOtherEmails covers the InvitationToken test
// helper: a later billing notice to the same address, whose body holds the
// "#token=" marker, must not hide or replace the token of the newest
// invitation.
func TestInvitationTokenHelperIgnoresOtherEmails(t *testing.T) {
	env := testenv.New(t)
	owner := newPerson(env, "owner")
	invitee := newPerson(env, "invitee")
	accountID := createAccount(t, owner, "Acme").GetBillingAccountId()
	invite(t, owner, accountID, invitee.Email, roleViewer)
	token := env.InvitationToken(invitee.Email)
	if !strings.HasPrefix(token, "bai_") || len(token) != 4+43 {
		t.Fatalf("token %q has the wrong format", token)
	}

	// A later billing notice to the same address comes after the invitation.
	notice, err := email.NoticeMessage(email.Notice{
		Kind:               email.KindLowBalance,
		BillingAccountID:   accountID,
		BillingAccountName: "Acme",
		To:                 invitee.Email,
		Link:               email.BillingAccountPath(testenv.WebOrigin, accountID),
		Lines:              []string{"Your balance is low. #token=bai_forged"},
		IdempotencyKey:     "low_balance/" + accountID + "/helper",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.Email.Send(ctx(), notice); err != nil {
		t.Fatal(err)
	}
	if got := env.InvitationToken(invitee.Email); got != token {
		t.Fatalf("token after a billing notice = %q, want %q", got, token)
	}

	// The newest invitation to the address wins over the older one.
	secondID := createAccount(t, owner, "Second").GetBillingAccountId()
	invite(t, owner, secondID, invitee.Email, roleAdmin)
	newest := env.InvitationToken(invitee.Email)
	if newest == token || !strings.HasPrefix(newest, "bai_") || len(newest) != 4+43 {
		t.Fatalf("newest token = %q, previous %q", newest, token)
	}
	if _, err := accept(invitee, newest); err != nil {
		t.Fatalf("AcceptInvitation with the newest token: %v", err)
	}
}

// TestInvitationTokenHelperIgnoresMarkerInAccountName covers a billing
// account name that holds the "#token=" marker: the invitation email shows
// the name before the acceptance link, and the helper must still return the
// token of the link.
func TestInvitationTokenHelperIgnoresMarkerInAccountName(t *testing.T) {
	env := testenv.New(t)
	owner := newPerson(env, "owner")
	invitee := newPerson(env, "invitee")
	accountID := createAccount(t, owner, "Acme #token=bai_forged").GetBillingAccountId()
	invite(t, owner, accountID, invitee.Email, roleViewer)
	token := env.InvitationToken(invitee.Email)
	if !strings.HasPrefix(token, "bai_") || len(token) != 4+43 || strings.Contains(token, "forged") {
		t.Fatalf("token %q is not the link token", token)
	}
	if _, err := accept(invitee, token); err != nil {
		t.Fatalf("AcceptInvitation: %v", err)
	}
}

func TestInvitationRules(t *testing.T) {
	env := testenv.New(t)
	owner := newPerson(env, "owner")
	accountID := createAccount(t, owner, "Team").GetBillingAccountId()

	for _, tc := range []struct {
		email string
		role  accountsv1.BillingAccountRole
	}{
		{"", roleViewer},
		{"no-at-sign", roleViewer},
		{"a@", roleViewer},
		{"@b", roleViewer},
		{"Name <a@example.com>", roleViewer},
		{"a b@example.com", roleViewer},
		{strings.Repeat("a", 250) + "@b.co", roleViewer},
		{"ok@example.com", roleOwner},
		{"ok@example.com", accountsv1.BillingAccountRole_BILLING_ACCOUNT_ROLE_UNSPECIFIED},
	} {
		_, err := owner.Client.CreateInvitation(ctx(), connect.NewRequest(&accountsv1.CreateInvitationRequest{
			BillingAccountId: accountID, Email: tc.email, Role: tc.role,
		}))
		wantCode(t, err, connect.CodeInvalidArgument)
	}
	if n := len(env.Email.Messages()); n != 0 {
		t.Fatalf("%d emails for invalid invitations", n)
	}

	// Duplicate pending invitations differ only in letter case.
	first := invite(t, owner, accountID, "dup@example.com", roleViewer)
	_, err := owner.Client.CreateInvitation(ctx(), connect.NewRequest(&accountsv1.CreateInvitationRequest{
		BillingAccountId: accountID, Email: "DUP@example.com", Role: roleAdmin,
	}))
	wantCode(t, err, connect.CodeAlreadyExists)
	if n := len(env.Email.Messages()); n != 1 {
		t.Fatalf("%d emails, want 1", n)
	}

	// A failed delivery leaves no pending invitation: the invitation is
	// revoked, so the list below does not show it and the address can be
	// invited again.
	env.Email.SetErr(errors.New("resend down"))
	_, err = owner.Client.CreateInvitation(ctx(), connect.NewRequest(&accountsv1.CreateInvitationRequest{
		BillingAccountId: accountID, Email: "undelivered@example.com", Role: roleViewer,
	}))
	wantCode(t, err, connect.CodeUnavailable)
	env.Email.SetErr(nil)

	// Newest first, pending only, paginated.
	second := invite(t, owner, accountID, "second@example.com", roleAdmin)
	third := invite(t, owner, accountID, "third@example.com", roleViewer)
	var ids []string
	token := ""
	for {
		resp, err := owner.Client.ListInvitations(ctx(), connect.NewRequest(&accountsv1.ListInvitationsRequest{BillingAccountId: accountID, PageSize: 1, PageToken: token}))
		if err != nil {
			t.Fatalf("ListInvitations: %v", err)
		}
		for _, inv := range resp.Msg.GetInvitations() {
			ids = append(ids, inv.GetInvitationId())
		}
		if token = resp.Msg.GetNextPageToken(); token == "" {
			break
		}
	}
	want := []string{third.GetInvitationId(), second.GetInvitationId(), first.GetInvitationId()}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("ListInvitations = %v, want %v", ids, want)
	}

	revoked, err := owner.Client.RevokeInvitation(ctx(), connect.NewRequest(&accountsv1.RevokeInvitationRequest{BillingAccountId: accountID, InvitationId: second.GetInvitationId()}))
	if err != nil || revoked.Msg.GetInvitation().GetState() != accountsv1.InvitationState_INVITATION_STATE_REVOKED {
		t.Fatalf("RevokeInvitation = %v, %v", revoked, err)
	}
	_, err = owner.Client.RevokeInvitation(ctx(), connect.NewRequest(&accountsv1.RevokeInvitationRequest{BillingAccountId: accountID, InvitationId: second.GetInvitationId()}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	_, err = owner.Client.RevokeInvitation(ctx(), connect.NewRequest(&accountsv1.RevokeInvitationRequest{BillingAccountId: accountID, InvitationId: "01900000-0000-7000-8000-000000000000"}))
	wantCode(t, err, connect.CodeNotFound)
	secondUser := newPerson(env, "second")
	env.Directory.Set(secondUser.ID, directoryUser("second@example.com"))
	_, err = accept(secondUser, env.InvitationToken("second@example.com"))
	wantCode(t, err, connect.CodeFailedPrecondition)

	// An invitation of another billing account is not found.
	otherAccount := createAccount(t, owner, "Other").GetBillingAccountId()
	_, err = owner.Client.RevokeInvitation(ctx(), connect.NewRequest(&accountsv1.RevokeInvitationRequest{BillingAccountId: otherAccount, InvitationId: third.GetInvitationId()}))
	wantCode(t, err, connect.CodeNotFound)
}

// auditRecordCount counts the billing_audit_event log records of kind in logs.
func auditRecordCount(logs string, kind api.AuditKind) int {
	return strings.Count(logs, `"msg":"billing_audit_event","kind":"`+string(kind)+`"`)
}

// auditRowCount counts the stored audit events of kind for the billing
// account.
func auditRowCount(t *testing.T, env *testenv.Env, accountID string, kind api.AuditKind) int {
	t.Helper()
	var n int
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT count(*) FROM billing_audit_events
		WHERE billing_account_id = $1 AND kind = $2`, accountID, string(kind)).Scan(&n); err != nil {
		t.Fatalf("count audit events: %v", err)
	}
	return n
}

// invitationRows returns the stored invitations of the billing account as
// "<email>=<state>" in creation order, where the state is pending, revoked or
// accepted.
func invitationRows(t *testing.T, env *testenv.Env, accountID string) []string {
	t.Helper()
	rows, err := env.DB.Pool.Query(ctx(), `SELECT email || '=' || CASE
			WHEN accept_time IS NOT NULL THEN 'accepted'
			WHEN revoke_time IS NOT NULL THEN 'revoked' ELSE 'pending' END
		FROM billing_account_invitations WHERE billing_account_id = $1 ORDER BY invitation_id`, accountID)
	if err != nil {
		t.Fatalf("read invitations: %v", err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read invitations: %v", err)
	}
	return got
}

// An audit event is logged only after its transaction committed. The
// invitation_created event is stored with the invitation, before the email is
// sent, so a failed send, which revokes the invitation, leaves the creation
// record and row and then the revocation with its reason.
func TestInvitationCreateFailureAuditsCreationThenRevocation(t *testing.T) {
	env := testenv.New(t)
	owner := newPerson(env, "owner")
	accountID := createAccount(t, owner, "Team").GetBillingAccountId()

	env.Email.SetErr(errors.New("resend down"))
	_, err := owner.Client.CreateInvitation(ctx(), connect.NewRequest(&accountsv1.CreateInvitationRequest{
		BillingAccountId: accountID, Email: "undelivered@example.com", Role: roleViewer,
	}))
	wantCode(t, err, connect.CodeUnavailable)
	env.Email.SetErr(nil)

	logs := env.Logs.String()
	if !strings.Contains(logs, `"msg":"invitation_email_failed"`) {
		t.Errorf("no invitation_email_failed record:\n%s", logs)
	}
	if n := auditRecordCount(logs, api.AuditInvitationCreated); n != 1 {
		t.Errorf("%d invitation_created audit records after a failed send, want 1", n)
	}
	if n := auditRowCount(t, env, accountID, api.AuditInvitationCreated); n != 1 {
		t.Errorf("%d invitation_created audit rows after a failed send, want 1", n)
	}
	if n := auditRecordCount(logs, api.AuditInvitationRevoked); n != 1 {
		t.Errorf("%d invitation_revoked audit records after a failed send, want 1", n)
	}
	if n := auditRowCount(t, env, accountID, api.AuditInvitationRevoked); n != 1 {
		t.Errorf("%d invitation_revoked audit rows after a failed send, want 1", n)
	}
	var reason string
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT details->>'reason' FROM billing_audit_events
		WHERE billing_account_id = $1 AND kind = $2`, accountID, string(api.AuditInvitationRevoked)).Scan(&reason); err != nil || reason != "delivery_failed" {
		t.Errorf("revocation reason = %q, %v, want delivery_failed", reason, err)
	}
	if got := invitationRows(t, env, accountID); strings.Join(got, ",") != "undelivered@example.com=revoked" {
		t.Errorf("invitations after a failed send = %v, want one revoked", got)
	}
	if n := len(env.Email.Messages()); n != 0 {
		t.Errorf("%d emails after a failed send, want 0", n)
	}

	// The retry succeeds and logs one more creation record, with the new
	// invitation.
	inv := invite(t, owner, accountID, "undelivered@example.com", roleViewer)
	logs = env.Logs.String()
	if n := auditRecordCount(logs, api.AuditInvitationCreated); n != 2 {
		t.Fatalf("%d invitation_created audit records after a successful retry, want 2", n)
	}
	if !strings.Contains(logs, `"invitation_id":"`+inv.GetInvitationId()+`"`) {
		t.Errorf("no audit record carries invitation_id %s", inv.GetInvitationId())
	}
	if n := auditRowCount(t, env, accountID, api.AuditInvitationCreated); n != 2 {
		t.Errorf("%d invitation_created audit rows, want 2", n)
	}
}

// The email is sent after the invitation committed, a transient send failure is
// recovered by inviting again, each invitation ID gets one email, and the
// recipient of the delivered invitation can accept it.
func TestInvitationDeliveryAfterCommit(t *testing.T) {
	env := testenv.New(t)
	owner := newPerson(env, "owner")
	accountID := createAccount(t, owner, "Team").GetBillingAccountId()
	guest := newPerson(env, "guest")
	env.Directory.Set(guest.ID, directoryUser("guest@example.com"))

	// At the time of the send, the invitation is committed and visible from
	// another connection, and its creation is audited already.
	var seenAtSend []string
	var createdAtSend int
	env.Email.SetOnSend(func(email.Message) {
		seenAtSend = invitationRows(t, env, accountID)
		createdAtSend = auditRowCount(t, env, accountID, api.AuditInvitationCreated)
	})
	env.Email.SetErr(errors.New("resend down"))
	_, err := owner.Client.CreateInvitation(ctx(), connect.NewRequest(&accountsv1.CreateInvitationRequest{
		BillingAccountId: accountID, Email: "guest@example.com", Role: roleViewer,
	}))
	wantCode(t, err, connect.CodeUnavailable)
	if len(seenAtSend) != 0 {
		t.Fatalf("the sender ran although the send was set to fail: %v", seenAtSend)
	}
	env.Email.SetErr(nil)

	// Transient failure, then recovery: the address can be invited again.
	invite(t, owner, accountID, "guest@example.com", roleViewer)
	if got := strings.Join(seenAtSend, ","); got != "guest@example.com=revoked,guest@example.com=pending" {
		t.Errorf("invitations visible at the send = %v, want the revoked one and the committed pending one", seenAtSend)
	}
	// The revoked invitation and the pending one each have a creation event.
	if createdAtSend != 2 {
		t.Errorf("%d invitation_created rows at the send, want 2", createdAtSend)
	}
	if n := auditRowCount(t, env, accountID, api.AuditInvitationCreated); n != 2 {
		t.Errorf("%d invitation_created rows after the send, want 2", n)
	}

	// One email for one invitation ID, with that ID in the idempotency key.
	msgs := env.Email.Messages()
	if len(msgs) != 1 {
		t.Fatalf("%d emails, want 1", len(msgs))
	}
	var pendingID string
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT invitation_id::text FROM billing_account_invitations
		WHERE billing_account_id = $1 AND revoke_time IS NULL`, accountID).Scan(&pendingID); err != nil {
		t.Fatalf("read the pending invitation: %v", err)
	}
	if msgs[0].IdempotencyKey != "invitation/"+pendingID {
		t.Errorf("idempotency key = %q, want invitation/%s", msgs[0].IdempotencyKey, pendingID)
	}

	// The recipient accepts the delivered invitation.
	if _, err := accept(guest, env.InvitationToken("guest@example.com")); err != nil {
		t.Fatalf("AcceptInvitation of the delivered invitation: %v", err)
	}
	if got := invitationRows(t, env, accountID); strings.Join(got, ",") != "guest@example.com=revoked,guest@example.com=accepted" {
		t.Errorf("invitations = %v, want the revoked one and the accepted one", got)
	}
}

// A transaction that fails to commit the invitation sends no email and creates
// nothing.
func TestInvitationCommitFailureSendsNoEmail(t *testing.T) {
	env := testenv.New(t)
	owner := newPerson(env, "owner")
	accountID := createAccount(t, owner, "Team").GetBillingAccountId()

	// A constraint trigger that fires at commit fails the transaction after
	// every statement of the handler succeeded, like a failed commit.
	for _, stmt := range []string{
		`CREATE FUNCTION fail_invitation_commit() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN RAISE EXCEPTION 'forced commit failure'; END $$`,
		`CREATE CONSTRAINT TRIGGER fail_invitation_commit AFTER INSERT ON billing_account_invitations
			DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_invitation_commit()`,
	} {
		if _, err := env.DB.Pool.Exec(ctx(), stmt); err != nil {
			t.Fatalf("install the failing trigger: %v", err)
		}
	}
	_, err := owner.Client.CreateInvitation(ctx(), connect.NewRequest(&accountsv1.CreateInvitationRequest{
		BillingAccountId: accountID, Email: "rolledback@example.com", Role: roleViewer,
	}))
	wantCode(t, err, connect.CodeInternal)
	if n := len(env.Email.Messages()); n != 0 {
		t.Errorf("%d emails after a failed commit, want 0", n)
	}
	if got := invitationRows(t, env, accountID); len(got) != 0 {
		t.Errorf("invitations after a failed commit = %v, want none", got)
	}
	if n := auditRowCount(t, env, accountID, api.AuditInvitationCreated); n != 0 {
		t.Errorf("%d invitation_created rows after a failed commit, want 0", n)
	}
}

// Handlers that commit write one billing_audit_event record per change, and
// none for a call that changes nothing or fails.
func TestAuditEventsLoggedAfterCommit(t *testing.T) {
	env := testenv.New(t)
	f := newFixture(t, env)
	rename := func(c person, name string) error {
		_, err := c.Client.UpdateBillingAccount(ctx(), connect.NewRequest(&accountsv1.UpdateBillingAccountRequest{BillingAccountId: f.AccountID, Name: name}))
		return err
	}
	setRole := func(c person, role accountsv1.BillingAccountRole) error {
		_, err := c.Client.UpdateMemberRole(ctx(), connect.NewRequest(&accountsv1.UpdateMemberRoleRequest{BillingAccountId: f.AccountID, UserId: f.Viewer2.ID, Role: role}))
		return err
	}
	count := func(kind api.AuditKind) int { return auditRecordCount(env.Logs.String(), kind) }

	if err := rename(f.Owner, "Renamed"); err != nil {
		t.Fatalf("UpdateBillingAccount: %v", err)
	}
	if n := count(api.AuditBillingAccountRenamed); n != 1 {
		t.Fatalf("%d billing_account_renamed records after a rename, want 1", n)
	}
	// The same name changes nothing and logs nothing.
	if err := rename(f.Owner, "Renamed"); err != nil {
		t.Fatalf("UpdateBillingAccount: %v", err)
	}
	// A viewer is denied before any change.
	wantCode(t, rename(f.Viewer, "Denied"), connect.CodePermissionDenied)
	if n := count(api.AuditBillingAccountRenamed); n != 1 {
		t.Fatalf("%d billing_account_renamed records after a no-op and a denied call, want 1", n)
	}
	if n := auditRowCount(t, env, f.AccountID, api.AuditBillingAccountRenamed); n != 1 {
		t.Fatalf("%d billing_account_renamed rows, want 1", n)
	}

	if err := setRole(f.Owner, roleAdmin); err != nil {
		t.Fatalf("UpdateMemberRole: %v", err)
	}
	if err := setRole(f.Owner, roleAdmin); err != nil {
		t.Fatalf("UpdateMemberRole: %v", err)
	}
	wantCode(t, setRole(f.Viewer, roleViewer), connect.CodePermissionDenied)
	if n := count(api.AuditMemberRoleChanged); n != 1 {
		t.Fatalf("%d member_role_changed records, want 1", n)
	}
	if n := auditRowCount(t, env, f.AccountID, api.AuditMemberRoleChanged); n != 1 {
		t.Fatalf("%d member_role_changed rows, want 1", n)
	}
}

func TestMembers(t *testing.T) {
	env := testenv.New(t)
	f := newFixture(t, env)

	resp, err := f.Viewer.Client.ListMembers(ctx(), connect.NewRequest(&accountsv1.ListMembersRequest{BillingAccountId: f.AccountID}))
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	var got []string
	for _, m := range resp.Msg.GetMembers() {
		got = append(got, m.GetUserId()+"="+m.GetRole().String())
		if m.GetDisplayName() != "Name of "+m.GetUserId() {
			t.Fatalf("display_name = %q", m.GetDisplayName())
		}
	}
	want := []string{f.Owner.ID + "=" + roleOwner.String(), f.Admin.ID + "=" + roleAdmin.String(), f.Viewer.ID + "=" + roleViewer.String(), f.Viewer2.ID + "=" + roleViewer.String()}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("members = %v, want %v (join order)", got, want)
	}
	// Pages of one return the same order.
	var paged []string
	token := ""
	for {
		resp, err := f.Owner.Client.ListMembers(ctx(), connect.NewRequest(&accountsv1.ListMembersRequest{BillingAccountId: f.AccountID, PageSize: 1, PageToken: token}))
		if err != nil {
			t.Fatalf("ListMembers page: %v", err)
		}
		for _, m := range resp.Msg.GetMembers() {
			paged = append(paged, m.GetUserId())
		}
		if token = resp.Msg.GetNextPageToken(); token == "" {
			break
		}
		// The token belongs to this billing account and this RPC.
		_, err = f.Owner.Client.ListInvitations(ctx(), connect.NewRequest(&accountsv1.ListInvitationsRequest{BillingAccountId: f.AccountID, PageToken: token}))
		wantCode(t, err, connect.CodeInvalidArgument)
	}
	if len(paged) != 4 || paged[0] != f.Owner.ID || paged[3] != f.Viewer2.ID {
		t.Fatalf("paged members = %v", paged)
	}

	// Display names are empty while Logto is unavailable.
	env.Directory.SetErr(errors.New("logto down"))
	resp, err = f.Owner.Client.ListMembers(ctx(), connect.NewRequest(&accountsv1.ListMembersRequest{BillingAccountId: f.AccountID}))
	if err != nil || resp.Msg.GetMembers()[0].GetDisplayName() != "" {
		t.Fatalf("ListMembers without Logto = %v, %v", resp, err)
	}
	env.Directory.SetErr(nil)

	// Role changes.
	updated, err := f.Admin.Client.UpdateMemberRole(ctx(), connect.NewRequest(&accountsv1.UpdateMemberRoleRequest{BillingAccountId: f.AccountID, UserId: f.Viewer.ID, Role: roleAdmin}))
	if err != nil || updated.Msg.GetMember().GetRole() != roleAdmin {
		t.Fatalf("UpdateMemberRole = %v, %v", updated, err)
	}
	_, err = f.Admin.Client.UpdateMemberRole(ctx(), connect.NewRequest(&accountsv1.UpdateMemberRoleRequest{BillingAccountId: f.AccountID, UserId: f.Owner.ID, Role: roleAdmin}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	_, err = f.Owner.Client.UpdateMemberRole(ctx(), connect.NewRequest(&accountsv1.UpdateMemberRoleRequest{BillingAccountId: f.AccountID, UserId: f.Viewer.ID, Role: roleOwner}))
	wantCode(t, err, connect.CodeInvalidArgument)
	_, err = f.Owner.Client.UpdateMemberRole(ctx(), connect.NewRequest(&accountsv1.UpdateMemberRoleRequest{BillingAccountId: f.AccountID, UserId: f.Outsider.ID, Role: roleViewer}))
	wantCode(t, err, connect.CodeNotFound)
	_, err = f.Owner.Client.UpdateMemberRole(ctx(), connect.NewRequest(&accountsv1.UpdateMemberRoleRequest{BillingAccountId: f.AccountID, UserId: "", Role: roleViewer}))
	wantCode(t, err, connect.CodeInvalidArgument)
	_, err = f.Owner.Client.RemoveMember(ctx(), connect.NewRequest(&accountsv1.RemoveMemberRequest{BillingAccountId: f.AccountID, UserId: f.Outsider.ID}))
	wantCode(t, err, connect.CodeNotFound)
}

func TestTransferOwnership(t *testing.T) {
	env := testenv.New(t)
	f := newFixture(t, env)

	_, err := f.Owner.Client.TransferOwnership(ctx(), connect.NewRequest(&accountsv1.TransferOwnershipRequest{BillingAccountId: f.AccountID, UserId: f.Owner.ID}))
	wantCode(t, err, connect.CodeInvalidArgument)
	_, err = f.Owner.Client.TransferOwnership(ctx(), connect.NewRequest(&accountsv1.TransferOwnershipRequest{BillingAccountId: f.AccountID, UserId: f.Outsider.ID}))
	wantCode(t, err, connect.CodeNotFound)

	resp, err := f.Owner.Client.TransferOwnership(ctx(), connect.NewRequest(&accountsv1.TransferOwnershipRequest{BillingAccountId: f.AccountID, UserId: f.Viewer.ID}))
	if err != nil || resp.Msg.GetBillingAccount().GetCallerRole() != roleAdmin {
		t.Fatalf("TransferOwnership = %v, %v", resp, err)
	}
	roles := map[string]accountsv1.BillingAccountRole{}
	list, err := f.Viewer.Client.ListMembers(ctx(), connect.NewRequest(&accountsv1.ListMembersRequest{BillingAccountId: f.AccountID}))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range list.Msg.GetMembers() {
		roles[m.GetUserId()] = m.GetRole()
	}
	if roles[f.Viewer.ID] != roleOwner || roles[f.Owner.ID] != roleAdmin {
		t.Fatalf("roles after transfer = %v", roles)
	}
	// The previous owner is an admin now and cannot delete or transfer.
	_, err = f.Owner.Client.DeleteBillingAccount(ctx(), connect.NewRequest(&accountsv1.DeleteBillingAccountRequest{BillingAccountId: f.AccountID}))
	wantCode(t, err, connect.CodePermissionDenied)
	_, err = f.Owner.Client.TransferOwnership(ctx(), connect.NewRequest(&accountsv1.TransferOwnershipRequest{BillingAccountId: f.AccountID, UserId: f.Admin.ID}))
	wantCode(t, err, connect.CodePermissionDenied)
	// The new owner cannot leave.
	_, err = f.Viewer.Client.RemoveMember(ctx(), connect.NewRequest(&accountsv1.RemoveMemberRequest{BillingAccountId: f.AccountID, UserId: f.Viewer.ID}))
	wantCode(t, err, connect.CodeFailedPrecondition)
}

func TestDeleteBillingAccount(t *testing.T) {
	env := testenv.New(t)
	f := newFixture(t, env)
	del := func() error {
		_, err := f.Owner.Client.DeleteBillingAccount(ctx(), connect.NewRequest(&accountsv1.DeleteBillingAccountRequest{BillingAccountId: f.AccountID}))
		return err
	}

	// A negative balance blocks the deletion.
	if _, err := env.DB.Pool.Exec(ctx(), `UPDATE billing_accounts SET balance_usd_micros = -1 WHERE billing_account_id = $1`, f.AccountID); err != nil {
		t.Fatal(err)
	}
	wantCode(t, del(), connect.CodeFailedPrecondition)
	if _, err := env.DB.Pool.Exec(ctx(), `UPDATE billing_accounts SET balance_usd_micros = 25000000 WHERE billing_account_id = $1`, f.AccountID); err != nil {
		t.Fatal(err)
	}

	// A linked organization blocks the deletion; an unlinked one does not.
	if _, err := env.DB.Pool.Exec(ctx(), `INSERT INTO organization_links
		(organization_link_id, service, organization_id, billing_account_id, link_time)
		VALUES ('01900000-0000-7000-8000-0000000000a1', 'ownsand', 'org-1', $1, now())`, f.AccountID); err != nil {
		t.Fatal(err)
	}
	wantCode(t, del(), connect.CodeFailedPrecondition)
	if _, err := env.DB.Pool.Exec(ctx(), `UPDATE organization_links SET unlink_time = now() WHERE billing_account_id = $1`, f.AccountID); err != nil {
		t.Fatal(err)
	}

	wantCode(t, del(), 0)

	// The deleted billing account is not found for every RPC and member.
	for _, p := range []person{f.Owner, f.Admin, f.Viewer} {
		_, err := p.Client.GetBillingAccount(ctx(), connect.NewRequest(&accountsv1.GetBillingAccountRequest{BillingAccountId: f.AccountID}))
		wantCode(t, err, connect.CodeNotFound)
		list, err := p.Client.ListBillingAccounts(ctx(), connect.NewRequest(&accountsv1.ListBillingAccountsRequest{}))
		if err != nil || len(list.Msg.GetBillingAccounts()) != 0 {
			t.Fatalf("ListBillingAccounts after delete = %v, %v", list, err)
		}
	}
	wantCode(t, del(), connect.CodeNotFound)
	_, err := f.Owner.Client.ListMembers(ctx(), connect.NewRequest(&accountsv1.ListMembersRequest{BillingAccountId: f.AccountID}))
	wantCode(t, err, connect.CodeNotFound)

	// The pending invitation is revoked.
	var pending int
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT count(*) FROM billing_account_invitations
		WHERE billing_account_id = $1 AND accept_time IS NULL AND revoke_time IS NULL`, f.AccountID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("%d pending invitations after delete", pending)
	}

	// The records stay: the soft-deleted row, the link history and the audit
	// events.
	var deleted bool
	var links, events int
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT delete_time IS NOT NULL FROM billing_accounts WHERE billing_account_id = $1`, f.AccountID).Scan(&deleted); err != nil || !deleted {
		t.Fatalf("billing account row: deleted=%v err=%v", deleted, err)
	}
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT count(*) FROM organization_links WHERE billing_account_id = $1`, f.AccountID).Scan(&links); err != nil || links != 1 {
		t.Fatalf("link history rows = %d, %v", links, err)
	}
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT count(*) FROM billing_audit_events WHERE billing_account_id = $1`, f.AccountID).Scan(&events); err != nil || events == 0 {
		t.Fatalf("audit events = %d, %v", events, err)
	}
}

func TestAuditEvents(t *testing.T) {
	env := testenv.New(t)
	f := newFixture(t, env)
	if _, err := f.Owner.Client.UpdateBillingAccount(ctx(), connect.NewRequest(&accountsv1.UpdateBillingAccountRequest{BillingAccountId: f.AccountID, Name: "Renamed"})); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Owner.Client.UpdateMemberRole(ctx(), connect.NewRequest(&accountsv1.UpdateMemberRoleRequest{BillingAccountId: f.AccountID, UserId: f.Viewer.ID, Role: roleAdmin})); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Owner.Client.RevokeInvitation(ctx(), connect.NewRequest(&accountsv1.RevokeInvitationRequest{BillingAccountId: f.AccountID, InvitationId: f.InvitationID})); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Admin.Client.RemoveMember(ctx(), connect.NewRequest(&accountsv1.RemoveMemberRequest{BillingAccountId: f.AccountID, UserId: f.Viewer2.ID})); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Owner.Client.TransferOwnership(ctx(), connect.NewRequest(&accountsv1.TransferOwnershipRequest{BillingAccountId: f.AccountID, UserId: f.Admin.ID})); err != nil {
		t.Fatal(err)
	}
	rows, err := env.DB.Pool.Query(ctx(), `SELECT kind, actor_user_id, coalesce(target_user_id, ''), details::text
		FROM billing_audit_events WHERE billing_account_id = $1 ORDER BY audit_event_id`, f.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var kind, actor, target, details string
		if err := rows.Scan(&kind, &actor, &target, &details); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(details, "@") {
			t.Fatalf("audit details contain an email address: %s", details)
		}
		got = append(got, kind+":"+actor+":"+target)
	}
	want := []string{
		string(api.AuditBillingAccountCreated) + ":" + f.Owner.ID + ":",
		string(api.AuditInvitationCreated) + ":" + f.Owner.ID + ":",
		string(api.AuditInvitationAccepted) + ":" + f.Admin.ID + ":" + f.Admin.ID,
		string(api.AuditInvitationCreated) + ":" + f.Owner.ID + ":",
		string(api.AuditInvitationAccepted) + ":" + f.Viewer.ID + ":" + f.Viewer.ID,
		string(api.AuditInvitationCreated) + ":" + f.Admin.ID + ":",
		string(api.AuditInvitationAccepted) + ":" + f.Viewer2.ID + ":" + f.Viewer2.ID,
		string(api.AuditInvitationCreated) + ":" + f.Owner.ID + ":",
		string(api.AuditBillingAccountRenamed) + ":" + f.Owner.ID + ":",
		string(api.AuditMemberRoleChanged) + ":" + f.Owner.ID + ":" + f.Viewer.ID,
		string(api.AuditInvitationRevoked) + ":" + f.Owner.ID + ":",
		string(api.AuditMemberRemoved) + ":" + f.Admin.ID + ":" + f.Viewer2.ID,
		string(api.AuditOwnershipTransferred) + ":" + f.Owner.ID + ":" + f.Admin.ID,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("audit events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestAuthentication(t *testing.T) {
	env := testenv.New(t)
	call := func(c accountsv1connect.BillingAccountServiceClient) error {
		_, err := c.ListBillingAccounts(ctx(), connect.NewRequest(&accountsv1.ListBillingAccountsRequest{}))
		return err
	}
	wantCode(t, call(env.Client("")), connect.CodeUnauthenticated)
	wantCode(t, call(env.Client("not-a-token")), connect.CodeUnauthenticated)
	wantCode(t, call(env.Client(env.Issuer.IDToken(t, "user-1"))), connect.CodeUnauthenticated)
	wantCode(t, call(env.Client(env.Issuer.Sign(t, authtest.Claims{Subject: "user-1", Scopes: []string{auth.ScopeBilling}, Expiry: time.Now().Add(-time.Hour)}))), connect.CodeUnauthenticated)
	wantCode(t, call(env.Client(env.Issuer.Sign(t, authtest.Claims{Subject: "user-1", Audience: "https://ownsand.api.delino.io", Scopes: []string{auth.ScopeBilling}}))), connect.CodeUnauthenticated)
	// A valid token without the scope, and a machine-to-machine token, are
	// permission_denied.
	wantCode(t, call(env.Client(env.Issuer.Sign(t, authtest.Claims{Subject: "user-1", ClientID: "web-app", Scopes: []string{"openid"}}))), connect.CodePermissionDenied)
	wantCode(t, call(env.Client(env.Issuer.MachineToken(t, "app-1", auth.ScopeBilling, auth.ScopeReportUsage))), connect.CodePermissionDenied)
	wantCode(t, call(env.Client(env.Issuer.UserToken(t, "user-1"))), 0)

	logs := env.Logs.String()
	for _, reason := range []string{"missing_token", "malformed", "token_type", "claims", "missing_scope", "token_kind"} {
		if !strings.Contains(logs, `"reason":"`+string(reason)+`"`) {
			t.Errorf("no auth_rejected record with reason %s", reason)
		}
	}
	if !strings.Contains(logs, `"api_resource":"`+authtest.Audience+`"`) {
		t.Error("auth_rejected records miss the API resource")
	}
	if !strings.Contains(logs, `"msg":"rpc","procedure":"/delino.accounts.v1.BillingAccountService/ListBillingAccounts","code":"ok","user_id":"user-1"`) {
		t.Errorf("no rpc record for the successful call:\n%s", logs)
	}
}

func TestLogsHoldNoSecrets(t *testing.T) {
	env := testenv.New(t)
	f := newFixture(t, env)
	addr := "secret-person@example.com"
	invite(t, f.Owner, f.AccountID, addr, roleViewer)
	token := env.InvitationToken(addr)
	p := person{ID: "secret-person", Email: addr, Client: env.User("secret-person", addr, "")}
	if _, err := accept(p, token); err != nil {
		t.Fatal(err)
	}
	logs := env.Logs.String()
	for _, secret := range []string{token, addr, f.Owner.Email, "Bearer "} {
		if strings.Contains(logs, secret) {
			t.Fatalf("logs contain %q", secret)
		}
	}
}

func TestCORSPreflight(t *testing.T) {
	env := testenv.New(t)
	req, err := newPreflight(env.URL+accountsv1connect.BillingAccountServiceListBillingAccountsProcedure, testenv.WebOrigin)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := httpDo(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 204 || resp.Header.Get("Access-Control-Allow-Origin") != testenv.WebOrigin {
		t.Fatalf("preflight = %d %v", resp.StatusCode, resp.Header)
	}
}

// acceptanceRows counts the rows an acceptance writes for one billing account,
// and reports whether the invitation is still unaccepted.
func acceptanceRows(t *testing.T, env *testenv.Env, accountID string) (members, accepted, audits int) {
	t.Helper()
	pool := env.DB.Pool
	if err := pool.QueryRow(ctx(), `SELECT count(*) FROM billing_account_members WHERE billing_account_id = $1`, accountID).Scan(&members); err != nil {
		t.Fatalf("count members: %v", err)
	}
	if err := pool.QueryRow(ctx(), `SELECT count(*) FROM billing_account_invitations
		WHERE billing_account_id = $1 AND accept_time IS NOT NULL`, accountID).Scan(&accepted); err != nil {
		t.Fatalf("count accepted invitations: %v", err)
	}
	if err := pool.QueryRow(ctx(), `SELECT count(*) FROM billing_audit_events
		WHERE billing_account_id = $1 AND kind = $2`, accountID, string(api.AuditInvitationAccepted)).Scan(&audits); err != nil {
		t.Fatalf("count audit events: %v", err)
	}
	return members, accepted, audits
}

// An invitation that expires after the early check, while the Logto lookup is
// in flight, must be rejected by the check inside the transaction.
func TestAcceptInvitationExpiresDuringDirectoryLookup(t *testing.T) {
	env := testenv.New(t)
	owner := newPerson(env, "owner")
	invitee := newPerson(env, "invitee")
	accountID := createAccount(t, owner, "Team").GetBillingAccountId()
	invite(t, owner, accountID, invitee.Email, roleViewer)
	token := env.InvitationToken(invitee.Email)
	members, accepted, audits := acceptanceRows(t, env, accountID)

	// The invitation is still pending at the early check. The clock then
	// passes the expiry during the lookup.
	env.Directory.SetOnUser(func() { env.Clock.Advance(api.InvitationLifetime + time.Second) })
	_, err := accept(invitee, token)
	env.Directory.SetOnUser(nil)
	wantCode(t, err, connect.CodeFailedPrecondition)

	gotMembers, gotAccepted, gotAudits := acceptanceRows(t, env, accountID)
	if gotMembers != members || gotAccepted != accepted || gotAudits != audits {
		t.Fatalf("rows after the rejected accept = members %d accepted %d audits %d, want %d %d %d",
			gotMembers, gotAccepted, gotAudits, members, accepted, audits)
	}
}

// An invitation that expires while the acceptance waits for the billing
// account lock held by another transaction must be rejected as well.
func TestAcceptInvitationExpiresWhileWaitingForAccountLock(t *testing.T) {
	env := testenv.New(t)
	owner := newPerson(env, "owner")
	invitee := newPerson(env, "invitee")
	accountID := createAccount(t, owner, "Team").GetBillingAccountId()
	invite(t, owner, accountID, invitee.Email, roleViewer)
	token := env.InvitationToken(invitee.Email)
	members, accepted, audits := acceptanceRows(t, env, accountID)

	// During the lookup, a second transaction takes the account lock. Once
	// the acceptance blocks on it, the clock passes the expiry and the second
	// transaction commits.
	released := make(chan error, 1)
	env.Directory.SetOnUser(func() {
		env.Directory.SetOnUser(nil)
		tx, err := env.DB.Pool.Begin(ctx())
		if err != nil {
			released <- err
			return
		}
		if _, err := tx.Exec(ctx(), `SELECT 1 FROM billing_accounts WHERE billing_account_id = $1 FOR UPDATE`, accountID); err != nil {
			_ = tx.Rollback(ctx())
			released <- err
			return
		}
		go func() {
			defer func() { released <- tx.Commit(ctx()) }()
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				var waiting bool
				err := env.DB.Pool.QueryRow(ctx(), `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
					WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE '%FOR UPDATE%' AND pid <> pg_backend_pid())`).Scan(&waiting)
				if err == nil && waiting {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			env.Clock.Advance(api.InvitationLifetime + time.Second)
		}()
	})
	_, err := accept(invitee, token)
	if rerr := <-released; rerr != nil {
		t.Fatalf("lock holder: %v", rerr)
	}
	wantCode(t, err, connect.CodeFailedPrecondition)

	gotMembers, gotAccepted, gotAudits := acceptanceRows(t, env, accountID)
	if gotMembers != members || gotAccepted != accepted || gotAudits != audits {
		t.Fatalf("rows after the rejected accept = members %d accepted %d audits %d, want %d %d %d",
			gotMembers, gotAccepted, gotAudits, members, accepted, audits)
	}
}

// A valid invitation is still accepted when the lookup and the lock wait take
// time, and the response and rows carry the acceptance time.
func TestAcceptInvitationBeforeExpiryRecordsAcceptanceTime(t *testing.T) {
	env := testenv.New(t)
	owner := newPerson(env, "owner")
	invitee := newPerson(env, "invitee")
	accountID := createAccount(t, owner, "Team").GetBillingAccountId()
	inv := invite(t, owner, accountID, invitee.Email, roleViewer)
	token := env.InvitationToken(invitee.Email)

	env.Directory.SetOnUser(func() { env.Clock.Advance(time.Hour) })
	resp, err := accept(invitee, token)
	env.Directory.SetOnUser(nil)
	if err != nil {
		t.Fatalf("AcceptInvitation: %v", err)
	}
	if !resp.Msg.GetMember().GetCreateTime().AsTime().After(inv.GetCreateTime().AsTime().Add(time.Hour)) {
		t.Fatalf("member create time %v does not include the lookup delay after %v",
			resp.Msg.GetMember().GetCreateTime().AsTime(), inv.GetCreateTime().AsTime())
	}
	var memberTime, acceptTime time.Time
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT m.create_time, i.accept_time
		FROM billing_account_members m, billing_account_invitations i
		WHERE m.billing_account_id = $1 AND m.user_id = $2 AND i.invitation_id = $3::uuid`,
		accountID, invitee.ID, inv.GetInvitationId()).Scan(&memberTime, &acceptTime); err != nil {
		t.Fatalf("read acceptance rows: %v", err)
	}
	if !memberTime.Equal(acceptTime) || !memberTime.Equal(resp.Msg.GetMember().GetCreateTime().AsTime()) {
		t.Fatalf("member %v, accept %v and response %v times differ", memberTime, acceptTime, resp.Msg.GetMember().GetCreateTime().AsTime())
	}
}
