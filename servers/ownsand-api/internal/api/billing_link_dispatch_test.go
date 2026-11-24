// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/protos/delino/accounts/v1/accountsv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/accounts/accountstest"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// linkDispatchTrace controls the service clock around the actual Postgres
// queries. The lookup end hook runs after the ownership result was read.
type linkDispatchTrace struct {
	insert, lookupStart, lookupEnd func()
}

type linkDispatchLookupKey struct{}

func (q linkDispatchTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	switch {
	case strings.HasPrefix(data.SQL, "INSERT INTO billing_pending_unlinks") && strings.Contains(data.SQL, "link_attempt_id"):
		q.insert()
	case strings.HasPrefix(data.SQL, "SELECT EXISTS (SELECT 1 FROM billing_pending_unlinks") && strings.Contains(data.SQL, "link_call_deadline IS NOT NULL"):
		q.lookupStart()
		return context.WithValue(ctx, linkDispatchLookupKey{}, true)
	}
	return ctx
}

func (q linkDispatchTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if ctx.Value(linkDispatchLookupKey{}) != nil && data.Err == nil {
		q.lookupEnd()
	}
}

type linkDispatchClient struct {
	accountsv1connect.UsageServiceClient
	budget chan time.Duration
}

func (c linkDispatchClient) LinkOrganization(ctx context.Context, req *connect.Request[accountsv1.LinkOrganizationRequest]) (*connect.Response[accountsv1.LinkOrganizationResponse], error) {
	deadline, _ := ctx.Deadline()
	c.budget <- time.Until(deadline)
	return c.UsageServiceClient.LinkOrganization(ctx, req)
}

func TestBillingLinkDispatchAfterOwnershipLookup(t *testing.T) {
	const replacementID = "01900000-0000-7000-8000-00000000ff01"
	type ownershipChange int
	const (
		keepAttempt ownershipChange = iota
		removeBeforeLookup
		replaceBeforeLookup
		replaceAfterLookup
	)
	for _, tc := range []struct {
		name                string
		lookupStartSeconds  int
		lookupEndSeconds    int
		change              ownershipChange
		wantCode            connect.Code
		wantExpiry          bool
		wantRemainingBudget time.Duration
	}{
		{name: "at cutoff", lookupStartSeconds: 14, lookupEndSeconds: 15, wantCode: connect.CodeUnavailable, wantExpiry: true},
		{name: "after cutoff", lookupStartSeconds: 14, lookupEndSeconds: 16, wantCode: connect.CodeUnavailable, wantExpiry: true},
		{name: "replacement during expiry", lookupStartSeconds: 14, lookupEndSeconds: 16, change: replaceAfterLookup, wantCode: connect.CodeUnavailable, wantExpiry: true},
		{name: "missing at cutoff", lookupStartSeconds: 14, lookupEndSeconds: 15, change: removeBeforeLookup, wantCode: connect.CodeAborted},
		{name: "replaced at cutoff", lookupStartSeconds: 14, lookupEndSeconds: 15, change: replaceBeforeLookup, wantCode: connect.CodeAborted},
		{name: "before cutoff", lookupStartSeconds: 10, lookupEndSeconds: 12, wantCode: connect.CodeUnavailable, wantRemainingBudget: 3 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f *linkAttemptFixture
			var testDB *db.DB
			replace := func() {
				if _, err := testDB.Pool.Exec(t.Context(), "UPDATE billing_pending_unlinks SET link_attempt_id = $2 WHERE organization_id = $1", f.org.ID, replacementID); err != nil {
					t.Error(err)
				}
			}
			trace := linkDispatchTrace{
				insert: func() {
					f.env.Clock.Advance(time.Duration(tc.lookupStartSeconds) * time.Second)
				},
				lookupStart: func() {
					switch tc.change {
					case removeBeforeLookup:
						if _, err := testDB.Pool.Exec(t.Context(), "DELETE FROM billing_pending_unlinks WHERE organization_id = $1", f.org.ID); err != nil {
							t.Error(err)
						}
					case replaceBeforeLookup:
						replace()
					}
				},
				lookupEnd: func() {
					f.env.Clock.Advance(time.Duration(tc.lookupEndSeconds-tc.lookupStartSeconds) * time.Second)
					if tc.change == replaceAfterLookup {
						replace()
					}
				},
			}
			budgets := make(chan time.Duration, 1)
			f = newLinkAttemptFixture(t, "dispatch-owner", func(o *app.Options) {
				testDB = o.DB
				cfg := testDB.Pool.Config()
				cfg.ConnConfig.Tracer = trace
				pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(pool.Close)
				o.DB = &db.DB{Pool: pool}
				o.UsageClient = linkDispatchClient{UsageServiceClient: o.UsageClient, budget: budgets}
			})

			if tc.wantRemainingBudget > 0 {
				gate := f.fake.Hold(accountstest.MethodLinkOrganization)
				t.Cleanup(gate.Release)
			}
			if err := f.link(f.org.OwnerClient, billingAccountA); testenv.Code(err) != tc.wantCode {
				t.Fatalf("link = %v, want %v", err, tc.wantCode)
			}
			if logged := strings.Contains(f.env.Logs.String(), "billing_link_attempt_expired_before_dispatch"); logged != tc.wantExpiry {
				t.Errorf("expiration logged = %t, want %t", logged, tc.wantExpiry)
			}
			if tc.wantRemainingBudget > 0 {
				if calls := f.fake.Calls(accountstest.MethodLinkOrganization); calls != 1 {
					t.Fatalf("Accounts link calls = %d, want 1", calls)
				}
				if budget := <-budgets; budget <= 0 || budget > tc.wantRemainingBudget {
					t.Errorf("Accounts call budget = %v, want (0, %v]", budget, tc.wantRemainingBudget)
				}
				f.wantRows(t, 1, 1)
			} else {
				if calls := f.fake.Calls(accountstest.MethodLinkOrganization); calls != 0 || len(budgets) != 0 {
					t.Errorf("Accounts link calls = %d, client calls = %d, want none", calls, len(budgets))
				}
				switch tc.change {
				case replaceBeforeLookup, replaceAfterLookup:
					var got string
					if err := testDB.Pool.QueryRow(t.Context(), "SELECT link_attempt_id::text FROM billing_pending_unlinks WHERE organization_id = $1", f.org.ID).Scan(&got); err != nil || got != replacementID {
						t.Errorf("replacement attempt = %q, %v, want %q", got, err, replacementID)
					}
				default:
					f.wantRows(t, 0, 0)
				}
			}
			f.wantLocalLink(t, false)
		})
	}
}
