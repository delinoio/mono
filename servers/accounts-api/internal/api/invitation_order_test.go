// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"encoding/base64"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/servers/accounts-api/internal/testenv"
)

func TestListInvitationsCreationTimePagination(t *testing.T) {
	env := testenv.New(t)
	owner := newPerson(env, "owner")
	admin := newPerson(env, "admin")
	viewer := newPerson(env, "viewer")
	outsider := newPerson(env, "outsider")
	account := createAccount(t, owner, "Team").GetBillingAccountId()
	other := createAccount(t, owner, "Other").GetBillingAccountId()
	join(t, env, owner, account, admin, roleAdmin)
	join(t, env, owner, account, viewer, roleViewer)

	const (
		rpc     = "ListInvitations"
		newest  = "018d0000-0000-7000-8000-000000000001"
		tieLow  = "018d0000-0000-7000-8000-000000000002"
		tieHigh = "018d0000-0000-7000-8000-000000000003"
	)
	older := env.Clock.Now().Add(-time.Hour)
	newer := older.Add(time.Microsecond)
	expires := newer.Add(24 * time.Hour)
	seed := func(id, accountID string, created, expiry time.Time, revoked *time.Time) {
		t.Helper()
		addr := id + "@example.com"
		if _, err := env.DB.Pool.Exec(ctx(), `INSERT INTO billing_account_invitations
			(invitation_id, billing_account_id, token_sha256, email, email_key, role,
				inviter_user_id, create_time, expire_time, revoke_time)
			VALUES ($1, $2, sha256(convert_to($3, 'UTF8')), $3, $3, 'viewer', $4, $5, $6, $7)`,
			id, accountID, addr, owner.ID, created, expiry, revoked); err != nil {
			t.Fatal(err)
		}
	}
	seed(newest, account, newer, expires, nil)
	seed(tieLow, account, older, expires, nil)
	seed(tieHigh, account, older, expires, nil)
	later := newer.Add(time.Minute)
	seed("018d0000-0000-7000-8000-000000000004", account, later, later.Add(time.Minute), nil)
	seed("018d0000-0000-7000-8000-000000000005", account, later, expires, &later)
	seed("018d0000-0000-7000-8000-000000000006", other, later, expires, nil)

	list := func(p person, accountID, token string, size int32) (*accountsv1.ListInvitationsResponse, error) {
		resp, err := p.Client.ListInvitations(ctx(), connect.NewRequest(&accountsv1.ListInvitationsRequest{
			BillingAccountId: accountID, PageSize: size, PageToken: token,
		}))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}
	type cursor struct {
		RPC       string `json:"r"`
		Scope     string `json:"s"`
		After     string `json:"a"`
		AfterTime *int64 `json:"t,omitempty"`
	}
	decode := func(t *testing.T, token string) cursor {
		t.Helper()
		data, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil {
			t.Fatal(err)
		}
		var c cursor
		if err := json.Unmarshal(data, &c); err != nil {
			t.Fatal(err)
		}
		return c
	}

	traverse := func(t *testing.T, p person, token string, size int32, legacy bool) []string {
		t.Helper()
		var ids []string
		for range 4 {
			page, err := list(p, account, token, size)
			if err != nil {
				t.Fatal(err)
			}
			rows := page.GetInvitations()
			for _, row := range rows {
				if row.GetBillingAccountId() != account || row.GetState() != accountsv1.InvitationState_INVITATION_STATE_PENDING {
					t.Fatalf("unexpected invitation: %v", row)
				}
				ids = append(ids, row.GetInvitationId())
			}
			token = page.GetNextPageToken()
			if token == "" {
				return ids
			}
			if len(rows) == 0 {
				t.Fatal("empty page has a next token")
			}
			last := rows[len(rows)-1]
			c := decode(t, token)
			if c.RPC != rpc || c.Scope != account || c.After != last.GetInvitationId() {
				t.Fatalf("cursor = %+v, want a cursor for the last row %s", c, last.GetInvitationId())
			}
			if legacy {
				if c.AfterTime != nil {
					t.Fatal("legacy cursor contains a timestamp")
				}
			} else {
				want := last.GetCreateTime().AsTime().UnixMicro()
				if c.AfterTime == nil || *c.AfterTime != want {
					t.Fatalf("cursor timestamp does not match the persisted timestamp %d", want)
				}
			}
		}
		t.Fatal("traversal did not end")
		return nil
	}

	t.Run("fresh traversal", func(t *testing.T) {
		want := []string{newest, tieHigh, tieLow}
		for _, size := range []int32{0, 1, 2, 3, 200} {
			if got := traverse(t, owner, "", size, false); !slices.Equal(got, want) {
				t.Fatalf("size %d: invitations = %v, want %v", size, got, want)
			}
		}
		if got := traverse(t, admin, "", 1, false); !slices.Equal(got, want) {
			t.Fatalf("admin invitations = %v, want %v", got, want)
		}
		full, err := list(owner, account, "", int32(len(want)))
		if err != nil {
			t.Fatal(err)
		}
		if full.GetNextPageToken() != "" {
			t.Fatal("an exact final page returned a next token")
		}
	})
	body, err := json.Marshal(cursor{RPC: rpc, Scope: account, After: tieHigh})
	if err != nil {
		t.Fatal(err)
	}
	legacy := base64.RawURLEncoding.EncodeToString(body)
	t.Run("legacy traversal", func(t *testing.T) {
		want := []string{tieLow, newest}
		if got := traverse(t, owner, legacy, 1, true); !slices.Equal(got, want) {
			t.Fatalf("legacy invitations = %v, want %v", got, want)
		}
	})
	t.Run("access and cursor scope", func(t *testing.T) {
		first, err := list(owner, account, "", 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, token := range []string{legacy, first.GetNextPageToken()} {
			_, err = list(owner, other, token, 1)
			wantCode(t, err, connect.CodeInvalidArgument)
			_, err = list(viewer, account, token, 1)
			wantCode(t, err, connect.CodePermissionDenied)
			_, err = list(outsider, account, token, 1)
			wantCode(t, err, connect.CodeNotFound)
		}
		wrongRPC := forgedPageToken(t, "ListMembers", account, tieHigh, older.UnixMicro())
		_, err = list(owner, account, wrongRPC, 1)
		wantCode(t, err, connect.CodeInvalidArgument)
		invalidID := forgedPageToken(t, rpc, account, "not-a-uuid", older.UnixMicro())
		_, err = list(owner, account, invalidID, 1)
		wantCode(t, err, connect.CodeInvalidArgument)
	})
	requireNoInternalError(t, env)
}
