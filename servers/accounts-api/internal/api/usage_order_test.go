// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"connectrpc.com/connect"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/servers/accounts-api/internal/testenv"
)

func TestListManageableBillingAccountsOrder(t *testing.T) {
	t.Parallel()
	const (
		rpc    = "ListManageableBillingAccounts"
		userID = "fixture-user"
		a      = "019b76da-a800-7000-8000-000000000001"
		b      = "019b76da-a800-7001-8000-000000000001"
		c      = "019b76da-a800-7002-8000-000000000001"
	)
	earlier := time.Date(2026, 1, 1, 0, 0, 0, 2_000, time.UTC)
	later := earlier.Add(time.Microsecond)
	env := testenv.New(t)
	_, err := env.DB.Pool.Exec(ctx(), `INSERT INTO billing_accounts
		(billing_account_id, name, create_time, update_time, delete_time) VALUES
		($1, 'A', $4, $4, NULL),
		($3, 'C', $5, $5, NULL),
		($2, 'B', $5, $5, NULL),
		('019b76da-a800-7003-8000-000000000001', 'Viewer', $5, $5, NULL),
		('019b76da-a800-7004-8000-000000000001', 'Deleted', $5, $5, $4),
		('019b76da-a800-7005-8000-000000000001', 'Unrelated', $5, $5, NULL)`, a, b, c, later, earlier)
	if err != nil {
		t.Fatal(err)
	}
	_, err = env.DB.Pool.Exec(ctx(), `INSERT INTO billing_account_members
		(billing_account_id, user_id, role, create_time) VALUES
		($1, $4, 'owner', $5),
		($2, $4, 'admin', $5),
		($2, 'fixture-other', 'owner', $5),
		($3, $4, 'owner', $5),
		('019b76da-a800-7003-8000-000000000001', $4, 'viewer', $5),
		('019b76da-a800-7003-8000-000000000001', 'fixture-other', 'owner', $5),
		('019b76da-a800-7004-8000-000000000001', $4, 'owner', $5),
		('019b76da-a800-7005-8000-000000000001', 'fixture-other', 'owner', $5)`, a, b, c, userID, later)
	if err != nil {
		t.Fatal(err)
	}

	type cursor struct {
		RPC   string `json:"r"`
		User  string `json:"s"`
		ID    string `json:"a"`
		Micro *int64 `json:"t,omitempty"`
	}
	encode := func(t *testing.T, value cursor) string {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	list := func(user, token string) (*connect.Response[accountsv1.ListManageableBillingAccountsResponse], error) {
		return env.Service().ListManageableBillingAccounts(ctx(), connect.NewRequest(&accountsv1.ListManageableBillingAccountsRequest{
			UserId: user, PageToken: token, PageSize: 1,
		}))
	}
	legacyToken := encode(t, cursor{RPC: rpc, User: userID, ID: a})
	for _, tc := range []struct {
		name, initialToken string
		wantIDs            []string
	}{
		{"creation order", "", []string{b, c, a}},
		{"legacy ID order", legacyToken, []string{b, c}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token := tc.initialToken
			for i, wantID := range tc.wantIDs {
				resp, err := list(userID, token)
				if err != nil {
					t.Fatal(err)
				}
				wantRole := roleOwner
				if wantID == b {
					wantRole = roleAdmin
				}
				accounts := resp.Msg.GetBillingAccounts()
				if len(accounts) != 1 || accounts[0].GetBillingAccountId() != wantID || accounts[0].GetCallerRole() != wantRole {
					t.Fatalf("page %d = %v, want only %s with role %s", i+1, accounts, wantID, wantRole)
				}
				token = resp.Msg.GetNextPageToken()
				if i == len(tc.wantIDs)-1 {
					if token != "" {
						t.Fatal("last page returned a continuation token")
					}
					break
				}
				raw, err := base64.RawURLEncoding.DecodeString(token)
				if err != nil {
					t.Fatal(err)
				}
				var got cursor
				if err := json.Unmarshal(raw, &got); err != nil {
					t.Fatal(err)
				}
				if got.RPC != rpc || got.User != userID || got.ID != wantID {
					t.Fatalf("page %d cursor = %+v, want %s bound to the RPC and user", i+1, got, wantID)
				}
				if tc.initialToken == "" {
					if got.Micro == nil || *got.Micro != earlier.UnixMicro() {
						t.Fatalf("page %d cursor = %s, want timestamp %d", i+1, raw, earlier.UnixMicro())
					}
				} else if got.Micro != nil {
					t.Fatalf("legacy traversal returned a timestamp token: %s", raw)
				}
				if i == 0 {
					_, err := list("fixture-other", token)
					wantCode(t, err, connect.CodeInvalidArgument)
					got.RPC = "ListBillingAccounts"
					_, err = list(userID, encode(t, got))
					wantCode(t, err, connect.CodeInvalidArgument)
				}
			}
		})
	}
	_, err = list(userID, "x")
	wantCode(t, err, connect.CodeInvalidArgument)
	requireNoInternalError(t, env)
}
