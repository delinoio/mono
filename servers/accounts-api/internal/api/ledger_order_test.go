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

// Ledger entry IDs of TestListLedgerEntriesOrder. The IDs are not in time
// order: the oldest entry has the highest ID and the newest group has IDs
// between the other two, as when an entry's ID is generated after its
// business timestamp was captured.
const (
	ledgerOldest = "ffffffff-0000-7000-8000-000000000001"
	ledgerMiddle = "00000000-0000-7000-8000-000000000001"
	ledgerTie1   = "11111111-0000-7000-8000-000000000001"
	ledgerTie2   = "11111111-0000-7000-8000-000000000002"
	ledgerTie3   = "11111111-0000-7000-8000-000000000003"
)

func seedLedgerEntry(t *testing.T, env *testenv.Env, accountID, id string, createTime time.Time) {
	t.Helper()
	if _, err := env.DB.Pool.Exec(ctx(), `INSERT INTO ledger_entries (ledger_entry_id, billing_account_id, kind,
			amount_usd_micros, balance_after_usd_micros, create_time)
		VALUES ($1, $2, 'manual_top_up', 1000000, 1000000, $3)`, id, accountID, createTime); err != nil {
		t.Fatal(err)
	}
}

func legacyLedgerToken(t *testing.T, rpc, scope, after string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"r": rpc, "s": scope, "a": after})
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(body)
}

// TestListLedgerEntriesOrder covers the order (create_time DESC,
// ledger_entry_id DESC), its page tokens and the compatibility with the
// ID-only tokens that the previous release issued.
func TestListLedgerEntriesOrder(t *testing.T) {
	env := testenv.New(t)
	f := newFixture(t, env)
	other := createAccount(t, f.Owner, "Other").GetBillingAccountId()
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	seedLedgerEntry(t, env, f.AccountID, ledgerOldest, base)
	seedLedgerEntry(t, env, f.AccountID, ledgerMiddle, base.Add(time.Second))
	for _, id := range []string{ledgerTie2, ledgerTie1, ledgerTie3} {
		seedLedgerEntry(t, env, f.AccountID, id, base.Add(2*time.Second+123*time.Microsecond))
	}
	seedLedgerEntry(t, env, other, "ffffffff-0000-7000-8000-000000000002", base.Add(time.Hour))

	list := func(account, token string, size int32) (*accountsv1.ListLedgerEntriesResponse, error) {
		resp, err := env.Credit(f.Owner.ID).ListLedgerEntries(ctx(), connect.NewRequest(&accountsv1.ListLedgerEntriesRequest{
			BillingAccountId: account, PageSize: size, PageToken: token}))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}
	ids := func(resp *accountsv1.ListLedgerEntriesResponse) []string {
		var out []string
		for _, e := range resp.GetLedgerEntries() {
			out = append(out, e.GetLedgerEntryId())
		}
		return out
	}
	// traverse follows the tokens from the first page and returns the IDs in
	// the order of the pages.
	traverse := func(token string, size int32) []string {
		t.Helper()
		var out []string
		for pages := 0; pages < 10; pages++ {
			resp, err := list(f.AccountID, token, size)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, ids(resp)...)
			if resp.GetNextPageToken() == "" {
				return out
			}
			token = resp.GetNextPageToken()
		}
		t.Fatal("traversal does not end")
		return nil
	}
	want := []string{ledgerTie3, ledgerTie2, ledgerTie1, ledgerMiddle, ledgerOldest}

	t.Run("a fresh traversal orders by create time and then by descending ID", func(t *testing.T) {
		full, err := list(f.AccountID, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := ids(full); !slices.Equal(got, want) || full.GetNextPageToken() != "" {
			t.Fatalf("unpaginated order = %v (token %q), want %v", got, full.GetNextPageToken(), want)
		}
		for size := int32(1); size <= 5; size++ {
			if got := traverse("", size); !slices.Equal(got, want) {
				t.Fatalf("page size %d order = %v, want %v", size, got, want)
			}
		}
	})
	t.Run("a page of the exact size has no next token", func(t *testing.T) {
		resp, err := list(f.AccountID, "", 5)
		if err != nil {
			t.Fatal(err)
		}
		if resp.GetNextPageToken() != "" {
			t.Fatalf("token after the last entry = %q", resp.GetNextPageToken())
		}
	})
	t.Run("an ID-only token resumes in ID order", func(t *testing.T) {
		// The previous release issued tokens that carry only the ID of the
		// last entry. ledgerOldest has the highest ID, so the legacy
		// traversal continues with the entries that have lower IDs, and the
		// new traversal after the same position would end.
		legacy := legacyLedgerToken(t, "ListLedgerEntries", f.AccountID, ledgerOldest)
		got := traverse(legacy, 2)
		if wantLegacy := []string{ledgerTie3, ledgerTie2, ledgerTie1, ledgerMiddle}; !slices.Equal(got, wantLegacy) {
			t.Fatalf("legacy traversal = %v, want %v", got, wantLegacy)
		}
		first, err := list(f.AccountID, legacy, 2)
		if err != nil {
			t.Fatal(err)
		}
		body, err := base64.RawURLEncoding.DecodeString(first.GetNextPageToken())
		if err != nil {
			t.Fatal(err)
		}
		var tok map[string]any
		if err := json.Unmarshal(body, &tok); err != nil {
			t.Fatal(err)
		}
		if _, hasTime := tok["t"]; hasTime {
			t.Fatalf("a legacy traversal issued a time token: %v", tok)
		}
	})
	t.Run("a token of another account or RPC is invalid", func(t *testing.T) {
		resp, err := list(f.AccountID, "", 1)
		if err != nil {
			t.Fatal(err)
		}
		_, err = list(other, resp.GetNextPageToken(), 1)
		wantCode(t, err, connect.CodeInvalidArgument)
		_, err = list(f.AccountID, forgedPageToken(t, "ListInvoices", f.AccountID, ledgerMiddle, 5), 1)
		wantCode(t, err, connect.CodeInvalidArgument)
		_, err = list(f.AccountID, forgedPageToken(t, "ListLedgerEntries", f.AccountID, ledgerMiddle, -1), 1)
		wantCode(t, err, connect.CodeInvalidArgument)
	})
	t.Run("members read, outsiders do not", func(t *testing.T) {
		resp, err := env.Credit(f.Viewer.ID).ListLedgerEntries(ctx(), connect.NewRequest(&accountsv1.ListLedgerEntriesRequest{
			BillingAccountId: f.AccountID}))
		if err != nil || !slices.Equal(ids(resp.Msg), want) {
			t.Fatalf("viewer list = %v, %v", ids(resp.Msg), err)
		}
		_, err = env.Credit(f.Outsider.ID).ListLedgerEntries(ctx(), connect.NewRequest(&accountsv1.ListLedgerEntriesRequest{
			BillingAccountId: f.AccountID}))
		wantCode(t, err, connect.CodeNotFound)
	})
	requireNoInternalError(t, env)
}
