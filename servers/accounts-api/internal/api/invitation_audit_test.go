// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"strings"
	"testing"

	"connectrpc.com/connect"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/servers/accounts-api/internal/api"
	"github.com/delinoio/mono/servers/accounts-api/internal/email"
	"github.com/delinoio/mono/servers/accounts-api/internal/testenv"
)

// memberRowCount counts the members of the billing account, owner included.
func memberRowCount(t *testing.T, env *testenv.Env, accountID string) int {
	t.Helper()
	var n int
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT count(*) FROM billing_account_members
		WHERE billing_account_id = $1`, accountID).Scan(&n); err != nil {
		t.Fatalf("count members: %v", err)
	}
	return n
}

// installFailingCreationAudit makes every insert of an invitation_created
// audit event fail, like a failing audit write, while other audit events and
// the invitation insert itself still succeed.
func installFailingCreationAudit(t *testing.T, env *testenv.Env) {
	t.Helper()
	for _, stmt := range []string{
		`CREATE FUNCTION fail_invitation_created_audit() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				IF NEW.kind = 'invitation_created' THEN RAISE EXCEPTION 'forced audit failure'; END IF;
				RETURN NEW;
			END $$`,
		`CREATE TRIGGER fail_invitation_created_audit BEFORE INSERT ON billing_audit_events
			FOR EACH ROW EXECUTE FUNCTION fail_invitation_created_audit()`,
	} {
		if _, err := env.DB.Pool.Exec(ctx(), stmt); err != nil {
			t.Fatalf("install the failing audit trigger: %v", err)
		}
	}
}

// tokenOf reads the invitation token from the acceptance link of the message.
func tokenOf(t *testing.T, msg email.Message) string {
	t.Helper()
	linkPrefix := strings.TrimRight(testenv.WebOrigin, "/") + email.InvitationPath + "#token="
	for _, line := range strings.Split(msg.Text, "\n") {
		if token, ok := strings.CutPrefix(strings.TrimSpace(line), linkPrefix); ok {
			return token
		}
	}
	t.Fatalf("no acceptance link in the invitation email to %s", msg.To)
	return ""
}

// The invitation_created audit event is stored in the transaction that inserts
// the invitation, so a failing audit write fails the RPC before any email is
// sent. Nobody can accept an invitation that was never committed, so there is
// no committed membership next to an internal error and a missing creation
// audit. The address can be invited again once the audit works.
func TestInvitationCreationAuditFailureRollsBackInvitation(t *testing.T) {
	env := testenv.New(t)
	owner := newPerson(env, "owner")
	accountID := createAccount(t, owner, "Team").GetBillingAccountId()
	installFailingCreationAudit(t, env)

	_, err := owner.Client.CreateInvitation(ctx(), connect.NewRequest(&accountsv1.CreateInvitationRequest{
		BillingAccountId: accountID, Email: "guest@example.com", Role: roleViewer,
	}))
	wantCode(t, err, connect.CodeInternal)
	if n := len(env.Email.Messages()); n != 0 {
		t.Errorf("%d emails after a failed creation audit, want 0", n)
	}
	if got := invitationRows(t, env, accountID); len(got) != 0 {
		t.Errorf("invitations after a failed creation audit = %v, want none", got)
	}
	for _, kind := range []api.AuditKind{api.AuditInvitationCreated, api.AuditInvitationRevoked, api.AuditInvitationAccepted} {
		if n := auditRowCount(t, env, accountID, kind); n != 0 {
			t.Errorf("%d %s rows after a failed creation audit, want 0", n, kind)
		}
		if n := auditRecordCount(env.Logs.String(), kind); n != 0 {
			t.Errorf("%d %s log records after a failed creation audit, want 0", n, kind)
		}
	}
	if n := memberRowCount(t, env, accountID); n != 1 {
		t.Errorf("%d members after a failed creation audit, want only the owner", n)
	}

	if _, err := env.DB.Pool.Exec(ctx(), `DROP TRIGGER fail_invitation_created_audit ON billing_audit_events`); err != nil {
		t.Fatalf("remove the failing audit trigger: %v", err)
	}
	invite(t, owner, accountID, "guest@example.com", roleViewer)
	if got := strings.Join(invitationRows(t, env, accountID), ","); got != "guest@example.com=pending" {
		t.Errorf("invitations after the retry = %s, want one pending", got)
	}
	if n := auditRowCount(t, env, accountID, api.AuditInvitationCreated); n != 1 {
		t.Errorf("%d invitation_created rows after the retry, want 1", n)
	}
}

// A recipient who accepts while CreateInvitation is still sending the email
// (the invitation is committed and its creation audit is stored by then) gets
// the membership, and CreateInvitation still succeeds with its creation audit
// intact. Before the creation audit moved into the first transaction, a failing
// audit write after the send could not undo the acceptance and left the sender
// with an internal error; the audit write cannot fail at this point any more.
func TestInvitationAcceptedDuringSendKeepsCreationAudit(t *testing.T) {
	env := testenv.New(t)
	owner := newPerson(env, "owner")
	accountID := createAccount(t, owner, "Team").GetBillingAccountId()
	guest := newPerson(env, "guest")
	env.Directory.Set(guest.ID, directoryUser("guest@example.com"))

	var createdAtSend int
	var acceptErr error
	env.Email.SetOnSend(func(msg email.Message) {
		createdAtSend = auditRowCount(t, env, accountID, api.AuditInvitationCreated)
		_, acceptErr = accept(guest, tokenOf(t, msg))
	})
	resp, err := owner.Client.CreateInvitation(ctx(), connect.NewRequest(&accountsv1.CreateInvitationRequest{
		BillingAccountId: accountID, Email: "guest@example.com", Role: roleViewer,
	}))
	if err != nil {
		t.Fatalf("CreateInvitation with a concurrent acceptance: %v", err)
	}
	if acceptErr != nil {
		t.Fatalf("AcceptInvitation during the send: %v", acceptErr)
	}
	if createdAtSend != 1 {
		t.Errorf("%d invitation_created rows at the send, want 1: the creation audit is stored before the email", createdAtSend)
	}
	if resp.Msg.GetInvitation().GetEmail() != "guest@example.com" {
		t.Errorf("response invitation email = %q", resp.Msg.GetInvitation().GetEmail())
	}
	if got := strings.Join(invitationRows(t, env, accountID), ","); got != "guest@example.com=accepted" {
		t.Errorf("invitations = %s, want one accepted", got)
	}
	if n := memberRowCount(t, env, accountID); n != 2 {
		t.Errorf("%d members, want the owner and the guest", n)
	}
	if n := auditRowCount(t, env, accountID, api.AuditInvitationCreated); n != 1 {
		t.Errorf("%d invitation_created rows, want 1", n)
	}
	if n := auditRowCount(t, env, accountID, api.AuditInvitationAccepted); n != 1 {
		t.Errorf("%d invitation_accepted rows, want 1", n)
	}
}
