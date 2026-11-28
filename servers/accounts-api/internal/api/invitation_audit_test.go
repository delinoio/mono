// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
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
	t.Parallel()
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

func TestInvitationDeliveryFailureWithRevocationCommitFailure(t *testing.T) {
	t.Parallel()
	env := testenv.New(t)
	owner := newPerson(env, "owner")
	accountID := createAccount(t, owner, "Team").GetBillingAccountId()
	const address = "guest@example.com"
	const commitFailure = "forced invitation revocation commit failure"

	// Let the revocation update and audit insert run before failing at commit.
	// Manual revocations do not have the delivery_failed reason.
	for _, stmt := range []string{
		`CREATE FUNCTION fail_delivery_revocation_commit() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN RAISE EXCEPTION '` + commitFailure + `'; END $$`,
		`CREATE CONSTRAINT TRIGGER fail_delivery_revocation_commit AFTER INSERT ON billing_audit_events
			DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
			WHEN (NEW.kind = 'invitation_revoked' AND NEW.details->>'reason' = 'delivery_failed')
			EXECUTE FUNCTION fail_delivery_revocation_commit()`,
	} {
		if _, err := env.DB.Pool.Exec(ctx(), stmt); err != nil {
			t.Fatalf("install the deferred revocation trigger: %v", err)
		}
	}

	pendingID := func() string {
		t.Helper()
		resp, err := owner.Client.ListInvitations(ctx(), connect.NewRequest(&accountsv1.ListInvitationsRequest{BillingAccountId: accountID}))
		if err != nil {
			t.Fatalf("ListInvitations: %v", err)
		}
		invitations := resp.Msg.GetInvitations()
		if len(invitations) != 1 {
			t.Fatalf("ListInvitations returned %d invitations, want 1", len(invitations))
		}
		inv := invitations[0]
		if inv.GetInvitationId() == "" || inv.GetEmail() != address || inv.GetState() != accountsv1.InvitationState_INVITATION_STATE_PENDING {
			t.Fatalf("ListInvitations returned %v, want a pending invitation for %s", inv, address)
		}
		return inv.GetInvitationId()
	}
	wantCreationAudits := func(want int) {
		t.Helper()
		if n := auditRowCount(t, env, accountID, api.AuditInvitationCreated); n != want {
			t.Errorf("%d invitation_created audit rows, want %d", n, want)
		}
		if n := auditRecordCount(env.Logs.String(), api.AuditInvitationCreated); n != want {
			t.Errorf("%d invitation_created audit log records, want %d", n, want)
		}
	}

	request := &accountsv1.CreateInvitationRequest{BillingAccountId: accountID, Email: address, Role: roleViewer}
	env.Email.SetErr(errors.New("resend down"))
	_, err := owner.Client.CreateInvitation(ctx(), connect.NewRequest(request))
	wantCode(t, err, connect.CodeUnavailable)

	originalID := pendingID()
	if got := invitationRows(t, env, accountID); strings.Join(got, ",") != address+"=pending" {
		t.Errorf("stored invitations after failed revocation = %v, want one pending", got)
	}
	wantCreationAudits(1)
	var createdID string
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT invitation_id::text FROM billing_audit_events
		WHERE billing_account_id = $1 AND kind = $2`, accountID, api.AuditInvitationCreated).Scan(&createdID); err != nil || createdID != originalID {
		t.Errorf("creation audit invitation ID = %q, %v, want %q", createdID, err, originalID)
	}
	logs := env.Logs.String()
	if n := auditRowCount(t, env, accountID, api.AuditInvitationRevoked); n != 0 {
		t.Errorf("%d invitation_revoked audit rows after failed revocation, want 0", n)
	}
	if n := auditRecordCount(logs, api.AuditInvitationRevoked); n != 0 {
		t.Errorf("%d invitation_revoked audit log records after failed revocation, want 0", n)
	}
	if n := len(env.Email.Messages()); n != 0 {
		t.Errorf("%d emails after failed delivery, want 0", n)
	}

	var creationLogs, failureLogs int
	for line := range strings.SplitSeq(logs, "\n") {
		if line == "" {
			continue
		}
		var record struct {
			Message          string        `json:"msg"`
			Level            string        `json:"level"`
			Kind             api.AuditKind `json:"kind"`
			BillingAccountID string        `json:"billing_account_id"`
			InvitationID     string        `json:"invitation_id"`
			Reason           string        `json:"reason"`
			Error            string        `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode log record: %v", err)
		}
		switch record.Message {
		case "billing_audit_event":
			if record.Kind == api.AuditInvitationCreated && record.BillingAccountID == accountID && record.InvitationID == originalID {
				creationLogs++
			}
		case "invitation_revoke_failed":
			failureLogs++
			if record.Level != slog.LevelError.String() || record.BillingAccountID != accountID ||
				record.InvitationID != originalID || record.Reason != "delivery_failed" || !strings.Contains(record.Error, commitFailure) {
				t.Errorf("unexpected invitation_revoke_failed record: %s", line)
			}
		}
	}
	if creationLogs != 1 || failureLogs != 1 {
		t.Errorf("creation audit logs for invitation %s = %d, revocation failure logs = %d, want 1 of each", originalID, creationLogs, failureLogs)
	}

	env.Email.SetErr(nil)
	_, err = owner.Client.CreateInvitation(ctx(), connect.NewRequest(request))
	wantCode(t, err, connect.CodeAlreadyExists)
	if id := pendingID(); id != originalID {
		t.Errorf("pending invitation after duplicate request = %s, want %s", id, originalID)
	}
	wantCreationAudits(1)
	if n := len(env.Email.Messages()); n != 0 {
		t.Errorf("%d emails after duplicate request, want 0", n)
	}

	revoked, err := owner.Client.RevokeInvitation(ctx(), connect.NewRequest(&accountsv1.RevokeInvitationRequest{
		BillingAccountId: accountID, InvitationId: originalID,
	}))
	if err != nil {
		t.Fatalf("RevokeInvitation with the deferred trigger installed: %v", err)
	}
	if state := revoked.Msg.GetInvitation().GetState(); state != accountsv1.InvitationState_INVITATION_STATE_REVOKED {
		t.Errorf("RevokeInvitation state = %s, want revoked", state)
	}
	var manualAudits int
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT count(*) FROM billing_audit_events
		WHERE billing_account_id = $1 AND kind = $2 AND invitation_id = $3
		AND details->>'reason' IS DISTINCT FROM 'delivery_failed'`,
		accountID, api.AuditInvitationRevoked, originalID).Scan(&manualAudits); err != nil || manualAudits != 1 {
		t.Errorf("manual revocation audit rows for invitation %s = %d, %v, want 1", originalID, manualAudits, err)
	}

	newID := invite(t, owner, accountID, address, roleViewer).GetInvitationId()
	if id := pendingID(); id != newID || id == originalID {
		t.Errorf("pending invitation after reinvite = %s, want new invitation %s distinct from %s", id, newID, originalID)
	}
	wantCreationAudits(2)
	messages := env.Email.Messages()
	if len(messages) != 1 {
		t.Fatalf("%d emails after reinvite, want 1", len(messages))
	}
	if msg := messages[0]; msg.To != address || msg.IdempotencyKey != "invitation/"+newID {
		t.Errorf("reinvite email recipient and key = %q, %q, want %q, %q", msg.To, msg.IdempotencyKey, address, "invitation/"+newID)
	}
}

// A recipient who accepts while CreateInvitation is still sending the email
// (the invitation is committed and its creation audit is stored by then) gets
// the membership, and CreateInvitation still succeeds with its creation audit
// intact. Before the creation audit moved into the first transaction, a failing
// audit write after the send could not undo the acceptance and left the sender
// with an internal error; the audit write cannot fail at this point any more.
func TestInvitationAcceptedDuringSendKeepsCreationAudit(t *testing.T) {
	t.Parallel()
	env := testenv.New(t)
	owner := newPerson(env, "owner")
	accountID := createAccount(t, owner, "Team").GetBillingAccountId()
	guest := newPerson(env, "guest")
	env.Directory.Set(guest.ID, directoryUser("guest@example.com"))

	type sendResult struct {
		called        bool
		creationCount int
		acceptErr     error
	}
	var hookMu sync.Mutex
	var hookResult sendResult
	env.Email.SetOnSend(func(msg email.Message) {
		creationCount := auditRowCount(t, env, accountID, api.AuditInvitationCreated)
		_, acceptErr := accept(guest, tokenOf(t, msg))
		hookMu.Lock()
		hookResult = sendResult{called: true, creationCount: creationCount, acceptErr: acceptErr}
		hookMu.Unlock()
	})
	resp, err := owner.Client.CreateInvitation(ctx(), connect.NewRequest(&accountsv1.CreateInvitationRequest{
		BillingAccountId: accountID, Email: "guest@example.com", Role: roleViewer,
	}))
	if err != nil {
		t.Fatalf("CreateInvitation with a concurrent acceptance: %v", err)
	}
	hookMu.Lock()
	result := hookResult
	hookMu.Unlock()
	if !result.called {
		t.Fatal("the send hook did not run during CreateInvitation")
	}
	if result.acceptErr != nil {
		t.Fatalf("AcceptInvitation during the send: %v", result.acceptErr)
	}
	if result.creationCount != 1 {
		t.Errorf("%d invitation_created rows at the send, want 1: the creation audit is stored before the email", result.creationCount)
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
