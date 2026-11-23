// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/servers/accounts-api/internal/testenv"
)

// These tests use the limits that the contract documents: 30 invitations for
// each inviter and 5 for each address in 24 hours. They are written as
// literals on purpose, so a change of a limit constant fails them.
const (
	rateInviterLimit = 30
	rateEmailLimit   = 5
)

const rateMessage = "too many invitations; try again later"

func createInvitation(p person, accountID, addr string) (*accountsv1.BillingAccountInvitation, error) {
	resp, err := p.Client.CreateInvitation(ctx(), connect.NewRequest(&accountsv1.CreateInvitationRequest{
		BillingAccountId: accountID, Email: addr, Role: roleViewer,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetInvitation(), nil
}

func revokeInvitation(t *testing.T, p person, accountID, invitationID string) {
	t.Helper()
	if _, err := p.Client.RevokeInvitation(ctx(), connect.NewRequest(&accountsv1.RevokeInvitationRequest{
		BillingAccountId: accountID, InvitationId: invitationID,
	})); err != nil {
		t.Fatalf("RevokeInvitation: %v", err)
	}
}

// inviteAndRevoke creates and revokes an invitation, which does not reset a
// limit.
func inviteAndRevoke(t *testing.T, p person, accountID, addr string) {
	t.Helper()
	inv, err := createInvitation(p, accountID, addr)
	if err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}
	revokeInvitation(t, p, accountID, inv.GetInvitationId())
}

func totalInvitationRows(t *testing.T, env *testenv.Env) int {
	t.Helper()
	var n int
	if err := env.DB.Pool.QueryRow(ctx(), `SELECT count(*) FROM billing_account_invitations`).Scan(&n); err != nil {
		t.Fatalf("count invitations: %v", err)
	}
	return n
}

// wantRateLimited asserts the resource_exhausted error of a rejected call. The
// message is the same for both limits.
func wantRateLimited(t *testing.T, err error) {
	t.Helper()
	wantCode(t, err, connect.CodeResourceExhausted)
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) || connectErr.Message() != rateMessage {
		t.Fatalf("message of the rate limit error = %v, want %q", err, rateMessage)
	}
}

// rateLimitRecords returns the invitation_rate_limited log records.
func rateLimitRecords(t *testing.T, env *testenv.Env) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(env.Logs.String(), "\n") {
		if !strings.Contains(line, `"invitation_rate_limited"`) {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("parse log record: %v", err)
		}
		records = append(records, record)
	}
	return records
}

// The sixth invitation to one address in 24 hours fails, also when every
// earlier invitation was revoked. The failed call sends no email and stores no
// row. Invitations count for 24 hours.
func TestInvitationLimitPerAddress(t *testing.T) {
	env := testenv.New(t)
	owner := newPerson(env, "owner")
	accountID := createAccount(t, owner, "Team").GetBillingAccountId()

	for i := 0; i < rateEmailLimit; i++ {
		inviteAndRevoke(t, owner, accountID, "a@example.com")
	}
	_, err := createInvitation(owner, accountID, "a@example.com")
	wantRateLimited(t, err)
	if n := len(env.Email.Messages()); n != rateEmailLimit {
		t.Errorf("%d emails, want %d: the rejected call sends none", n, rateEmailLimit)
	}
	if n := totalInvitationRows(t, env); n != rateEmailLimit {
		t.Errorf("%d invitation rows, want %d: the rejected call stores none", n, rateEmailLimit)
	}
	// The key of an address ignores the case, so a variant counts the same.
	_, err = createInvitation(owner, accountID, "A@Example.com")
	wantRateLimited(t, err)
	// Another address is not affected.
	inviteAndRevoke(t, owner, accountID, "b@example.com")

	env.Clock.Advance(23 * time.Hour)
	_, err = createInvitation(owner, accountID, "a@example.com")
	wantRateLimited(t, err)

	env.Clock.Advance(time.Hour + time.Minute)
	if _, err := createInvitation(owner, accountID, "a@example.com"); err != nil {
		t.Fatalf("CreateInvitation after the invitations aged out: %v", err)
	}
}

// The address limit counts the invitations of every inviter and every billing
// account.
func TestInvitationLimitPerAddressAcrossInviters(t *testing.T) {
	env := testenv.New(t)
	first := newPerson(env, "first")
	second := newPerson(env, "second")
	firstAccount := createAccount(t, first, "First").GetBillingAccountId()
	secondAccount := createAccount(t, second, "Second").GetBillingAccountId()

	for i := 0; i < 3; i++ {
		inviteAndRevoke(t, first, firstAccount, "victim@example.com")
	}
	for i := 0; i < rateEmailLimit-3; i++ {
		inviteAndRevoke(t, second, secondAccount, "victim@example.com")
	}
	_, err := createInvitation(second, secondAccount, "victim@example.com")
	wantRateLimited(t, err)
	_, err = createInvitation(first, firstAccount, "victim@example.com")
	wantRateLimited(t, err)
	if n := len(env.Email.Messages()); n != rateEmailLimit {
		t.Errorf("%d emails, want %d", n, rateEmailLimit)
	}
}

// The inviter limit counts across billing accounts and revoked invitations.
func TestInvitationLimitPerInviterAcrossAccounts(t *testing.T) {
	env := testenv.New(t)
	owner := newPerson(env, "owner")
	var accounts []string
	for i := 0; i < 3; i++ {
		accounts = append(accounts, createAccount(t, owner, fmt.Sprintf("Team %d", i)).GetBillingAccountId())
	}
	for i := 0; i < rateInviterLimit; i++ {
		inviteAndRevoke(t, owner, accounts[i%len(accounts)], fmt.Sprintf("guest-%d@example.com", i))
	}
	for _, accountID := range accounts {
		_, err := createInvitation(owner, accountID, "one-more@example.com")
		wantRateLimited(t, err)
	}
	if n := len(env.Email.Messages()); n != rateInviterLimit {
		t.Errorf("%d emails, want %d", n, rateInviterLimit)
	}
	if n := totalInvitationRows(t, env); n != rateInviterLimit {
		t.Errorf("%d invitation rows, want %d", n, rateInviterLimit)
	}
	// Another inviter is not affected.
	other := newPerson(env, "other")
	otherAccount := createAccount(t, other, "Other").GetBillingAccountId()
	if _, err := createInvitation(other, otherAccount, "one-more@example.com"); err != nil {
		t.Fatalf("CreateInvitation of another inviter: %v", err)
	}

	env.Clock.Advance(24*time.Hour + time.Minute)
	if _, err := createInvitation(owner, accounts[0], "one-more@example.com"); err != nil {
		t.Fatalf("CreateInvitation after the invitations aged out: %v", err)
	}
}

// Concurrent calls of one inviter to different addresses cannot go above the
// limit, also across billing accounts, which hold different account locks.
func TestInvitationLimitConcurrentCalls(t *testing.T) {
	env := testenv.New(t)
	owner := newPerson(env, "owner")
	accounts := []string{
		createAccount(t, owner, "Team A").GetBillingAccountId(),
		createAccount(t, owner, "Team B").GetBillingAccountId(),
	}
	const calls = 40
	errs := make([]error, calls)
	var wg sync.WaitGroup
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = createInvitation(owner, accounts[i%len(accounts)], fmt.Sprintf("guest-%d@example.com", i))
		}()
	}
	wg.Wait()
	succeeded, limited := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case connect.CodeOf(err) == connect.CodeResourceExhausted:
			limited++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if succeeded != rateInviterLimit || limited != calls-rateInviterLimit {
		t.Errorf("%d calls succeeded and %d were limited, want %d and %d", succeeded, limited, rateInviterLimit, calls-rateInviterLimit)
	}
	if n := len(env.Email.Messages()); n != rateInviterLimit {
		t.Errorf("%d emails, want %d", n, rateInviterLimit)
	}
	if n := totalInvitationRows(t, env); n != rateInviterLimit {
		t.Errorf("%d invitation rows, want %d", n, rateInviterLimit)
	}
}

// Concurrent calls of different inviters to one address cannot go above the
// address limit either.
func TestInvitationLimitConcurrentCallsToOneAddress(t *testing.T) {
	env := testenv.New(t)
	const callers = 8
	people := make([]person, callers)
	accounts := make([]string, callers)
	for i := range people {
		people[i] = newPerson(env, "owner")
		accounts[i] = createAccount(t, people[i], "Team").GetBillingAccountId()
	}
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = createInvitation(people[i], accounts[i], "victim@example.com")
		}()
	}
	wg.Wait()
	succeeded := 0
	for _, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case connect.CodeOf(err) != connect.CodeResourceExhausted:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if succeeded != rateEmailLimit {
		t.Errorf("%d calls succeeded, want %d", succeeded, rateEmailLimit)
	}
	if n := len(env.Email.Messages()); n != rateEmailLimit {
		t.Errorf("%d emails, want %d", n, rateEmailLimit)
	}
}

// The rejection is logged with the account, the inviter and the limit kind,
// and never with the address.
func TestInvitationLimitLogHoldsNoAddress(t *testing.T) {
	env := testenv.New(t)
	owner := newPerson(env, "owner")
	accountID := createAccount(t, owner, "Team").GetBillingAccountId()
	const addr = "secret-recipient@example.com"
	for i := 0; i < rateEmailLimit; i++ {
		inviteAndRevoke(t, owner, accountID, addr)
	}
	if n := len(rateLimitRecords(t, env)); n != 0 {
		t.Fatalf("%d invitation_rate_limited records before a rejection, want 0", n)
	}
	_, err := createInvitation(owner, accountID, addr)
	wantRateLimited(t, err)

	records := rateLimitRecords(t, env)
	if len(records) != 1 {
		t.Fatalf("%d invitation_rate_limited records, want 1", len(records))
	}
	if got := records[0]["limit_kind"]; got != "email" {
		t.Errorf("limit_kind = %v, want email", got)
	}
	if records[0]["billing_account_id"] != accountID || records[0]["inviter_user_id"] != owner.ID {
		t.Errorf("record = %v, want the billing account %s and the inviter %s", records[0], accountID, owner.ID)
	}
	line := ""
	for _, l := range strings.Split(env.Logs.String(), "\n") {
		if strings.Contains(l, `"invitation_rate_limited"`) {
			line = l
		}
	}
	if strings.Contains(strings.ToLower(env.Logs.String()), "secret-recipient") {
		t.Errorf("the logs hold the address: %s", line)
	}

	// The inviter limit logs its own kind.
	other := newPerson(env, "other")
	otherAccount := createAccount(t, other, "Other").GetBillingAccountId()
	for i := 0; i < rateInviterLimit; i++ {
		if _, err := createInvitation(other, otherAccount, fmt.Sprintf("guest-%d@example.com", i)); err != nil {
			t.Fatalf("CreateInvitation: %v", err)
		}
	}
	_, err = createInvitation(other, otherAccount, "last@example.com")
	wantRateLimited(t, err)
	records = rateLimitRecords(t, env)
	if len(records) != 2 || records[1]["limit_kind"] != "inviter" {
		t.Errorf("records = %v, want the second to have limit_kind inviter", records)
	}
}

// An invitation whose email could not be sent is revoked by the compensating
// transaction and still counts, so failing sends cannot reset a limit. A
// pending duplicate keeps the already_exists error and counts nothing.
func TestInvitationLimitCountsFailedSendsAndKeepsDuplicateError(t *testing.T) {
	env := testenv.New(t)
	owner := newPerson(env, "owner")
	accountID := createAccount(t, owner, "Team").GetBillingAccountId()

	env.Email.SetErr(errors.New("resend down"))
	for i := 0; i < rateEmailLimit; i++ {
		_, err := createInvitation(owner, accountID, "a@example.com")
		wantCode(t, err, connect.CodeUnavailable)
	}
	env.Email.SetErr(nil)
	_, err := createInvitation(owner, accountID, "a@example.com")
	wantRateLimited(t, err)

	invite(t, owner, accountID, "b@example.com", roleViewer)
	_, err = createInvitation(owner, accountID, "b@example.com")
	wantCode(t, err, connect.CodeAlreadyExists)
	if n := totalInvitationRows(t, env); n != rateEmailLimit+1 {
		t.Errorf("%d invitation rows, want %d", n, rateEmailLimit+1)
	}
}

// Invitations below the limits are not affected: an owner revokes and invites
// again, and invites the address again after the invitation expired.
func TestInvitationLimitLeavesNormalFlowsAlone(t *testing.T) {
	env := testenv.New(t)
	owner := newPerson(env, "owner")
	accountID := createAccount(t, owner, "Team").GetBillingAccountId()

	inviteAndRevoke(t, owner, accountID, "a@example.com")
	invite(t, owner, accountID, "a@example.com", roleViewer)

	env.Clock.Advance(8 * 24 * time.Hour)
	invite(t, owner, accountID, "a@example.com", roleViewer)
	if got := strings.Join(invitationRows(t, env, accountID), ","); !strings.Contains(got, "a@example.com=pending") {
		t.Errorf("invitations = %s, want a pending invitation", got)
	}
	if n := len(rateLimitRecords(t, env)); n != 0 {
		t.Errorf("%d invitation_rate_limited records, want 0", n)
	}
}
