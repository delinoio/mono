// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"connectrpc.com/connect"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/servers/accounts-api/internal/testenv"
)

// forgedPageToken builds a page token the way a client that decodes the
// opaque value could: tokens are not signed, so any position can be sent.
func forgedPageToken(t *testing.T, rpc, scope, after string, afterTime int64) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"r": rpc, "s": scope, "a": after, "t": afterTime})
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(body)
}

// TestForgedPageTokenPositions covers page tokens whose decoded position a
// client chose: an out-of-range time or a NUL byte is invalid_argument and
// never reaches Postgres, while the positions that the server issues work.
func TestForgedPageTokenPositions(t *testing.T) {
	env := testenv.New(t)
	f := newFixture(t, env)
	org := newOrg(t)
	year, month := 2026, 1
	summaryScope := fmt.Sprintf("%s/%04d-%02d", f.AccountID, year, month)

	listMembers := func(token string) error {
		_, err := f.Owner.Client.ListMembers(ctx(), connect.NewRequest(&accountsv1.ListMembersRequest{
			BillingAccountId: f.AccountID, PageToken: token}))
		return err
	}
	listInvoices := func(token string) error {
		_, err := env.Credit(f.Owner.ID).ListInvoices(ctx(), connect.NewRequest(&accountsv1.ListInvoicesRequest{
			BillingAccountId: f.AccountID, PageToken: token}))
		return err
	}
	listLinked := func(token string) error {
		_, err := env.BillingUsage(f.Owner.ID).ListLinkedOrganizations(ctx(), connect.NewRequest(&accountsv1.ListLinkedOrganizationsRequest{
			BillingAccountId: f.AccountID, PageToken: token}))
		return err
	}
	listSummary := func(token string) error {
		_, err := env.BillingUsage(f.Owner.ID).ListUsageSummary(ctx(), connect.NewRequest(&accountsv1.ListUsageSummaryRequest{
			BillingAccountId: f.AccountID, Year: int32(year), Month: int32(month), PageToken: token}))
		return err
	}

	t.Run("negative time", func(t *testing.T) {
		for _, negative := range []int64{-1, -1_000_000, math.MinInt64} {
			wantCode(t, listMembers(forgedPageToken(t, "ListMembers", f.AccountID, f.Owner.ID, negative)), connect.CodeInvalidArgument)
			wantCode(t, listInvoices(forgedPageToken(t, "ListInvoices", f.AccountID, org, negative)), connect.CodeInvalidArgument)
			wantCode(t, listLinked(forgedPageToken(t, "ListLinkedOrganizations", f.AccountID, org, negative)), connect.CodeInvalidArgument)
		}
	})
	t.Run("NUL byte in the position", func(t *testing.T) {
		for _, after := range []string{"\x00", "abc\x00", "\x00abc"} {
			wantCode(t, listMembers(forgedPageToken(t, "ListMembers", f.AccountID, after, 5)), connect.CodeInvalidArgument)
			wantCode(t, listSummary(forgedPageToken(t, "ListUsageSummary", summaryScope, "ownsand\n"+after+"\nmeter", 0)), connect.CodeInvalidArgument)
			wantCode(t, listSummary(forgedPageToken(t, "ListUsageSummary", summaryScope, after, 0)), connect.CodeInvalidArgument)
		}
	})
	t.Run("positions the server issues", func(t *testing.T) {
		// A join time of the Unix epoch and a far future time are valid
		// positions of ListMembers.
		wantCode(t, listMembers(forgedPageToken(t, "ListMembers", f.AccountID, f.Owner.ID, 0)), 0)
		far := time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC).UnixMicro()
		wantCode(t, listMembers(forgedPageToken(t, "ListMembers", f.AccountID, f.Owner.ID, far)), 0)
		wantCode(t, listInvoices(forgedPageToken(t, "ListInvoices", f.AccountID, org, far)), 0)
		wantCode(t, listLinked(forgedPageToken(t, "ListLinkedOrganizations", f.AccountID, org, far)), 0)
		wantCode(t, listSummary(forgedPageToken(t, "ListUsageSummary", summaryScope, "ownsand\n"+org+"\nmeter", 0)), 0)
	})
	requireNoInternalError(t, env)
}
