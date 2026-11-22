// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"strings"
	"testing"

	"connectrpc.com/connect"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/servers/accounts-api/internal/testenv"
)

// requireNoInternalError fails when the server logged an internal error,
// which an input that a client controls must never cause.
func requireNoInternalError(t *testing.T, env *testenv.Env) {
	t.Helper()
	var internal []string
	for _, line := range strings.Split(env.Logs.String(), "\n") {
		if strings.Contains(line, "rpc_internal_error") {
			internal = append(internal, line)
		}
	}
	if len(internal) > 0 {
		t.Fatalf("an RPC logged rpc_internal_error:\n%s", strings.Join(internal, "\n"))
	}
}

// TestUserIDRejectsNULBytes covers every RPC that takes a user_id of a
// request: a NUL byte is invalid_argument and never reaches Postgres, which
// cannot store U+0000 in text. Other unknown IDs keep their results.
func TestUserIDRejectsNULBytes(t *testing.T) {
	env := testenv.New(t)
	f := newFixture(t, env)
	org := newOrg(t)

	type call struct {
		name string
		do   func(userID string) error
		// unknown is the code of an unknown, well-formed user ID.
		unknown connect.Code
	}
	calls := []call{
		{"UpdateMemberRole", func(userID string) error {
			_, err := f.Owner.Client.UpdateMemberRole(ctx(), connect.NewRequest(&accountsv1.UpdateMemberRoleRequest{
				BillingAccountId: f.AccountID, UserId: userID, Role: roleViewer}))
			return err
		}, connect.CodeNotFound},
		{"RemoveMember", func(userID string) error {
			_, err := f.Owner.Client.RemoveMember(ctx(), connect.NewRequest(&accountsv1.RemoveMemberRequest{
				BillingAccountId: f.AccountID, UserId: userID}))
			return err
		}, connect.CodeNotFound},
		{"TransferOwnership", func(userID string) error {
			_, err := f.Owner.Client.TransferOwnership(ctx(), connect.NewRequest(&accountsv1.TransferOwnershipRequest{
				BillingAccountId: f.AccountID, UserId: userID}))
			return err
		}, connect.CodeNotFound},
		{"LinkOrganization", func(userID string) error {
			_, err := env.Service().LinkOrganization(ctx(), connect.NewRequest(&accountsv1.LinkOrganizationRequest{
				Service: ownsand, OrganizationId: org, BillingAccountId: f.AccountID, UserId: userID}))
			return err
		}, connect.CodeNotFound},
		{"ListManageableBillingAccounts", func(userID string) error {
			_, err := env.Service().ListManageableBillingAccounts(ctx(), connect.NewRequest(&accountsv1.ListManageableBillingAccountsRequest{
				UserId: userID}))
			return err
		}, 0},
	}
	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			for _, nul := range []string{"\x00", "abc\x00", "\x00abc", "ab\x00cd"} {
				wantCode(t, c.do(nul), connect.CodeInvalidArgument)
			}
			wantCode(t, c.do(""), connect.CodeInvalidArgument)
			// Other control characters are valid text and stay ordinary IDs.
			wantCode(t, c.do("abc\x01"), c.unknown)
			wantCode(t, c.do("no-such-user"), c.unknown)
		})
	}
	requireNoInternalError(t, env)
}
