// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"errors"
	"strings"
	"sync"
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
	insert, lookupEnd func()
	lookupStart       func(context.Context)
}

type linkDispatchLookupKey struct{}

func (q linkDispatchTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	switch {
	case strings.HasPrefix(data.SQL, "INSERT INTO billing_pending_unlinks") && strings.Contains(data.SQL, "link_attempt_id"):
		q.insert()
	case strings.HasPrefix(data.SQL, "SELECT EXISTS (SELECT 1 FROM billing_pending_unlinks") && strings.Contains(data.SQL, "link_call_deadline IS NOT NULL"):
		q.lookupStart(ctx)
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
	recorder *linkDispatchRecorder
}

type linkDispatchObservation struct {
	calls       int
	firstBudget time.Duration
}

type linkDispatchRecorder struct {
	mu          sync.Mutex
	observation linkDispatchObservation
}

func (r *linkDispatchRecorder) record(budget time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.observation.calls == 0 {
		r.observation.firstBudget = budget
	}
	r.observation.calls++
}

func (r *linkDispatchRecorder) snapshot() linkDispatchObservation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.observation
}

func (c linkDispatchClient) LinkOrganization(ctx context.Context, req *connect.Request[accountsv1.LinkOrganizationRequest]) (*connect.Response[accountsv1.LinkOrganizationResponse], error) {
	deadline, _ := ctx.Deadline()
	c.recorder.record(time.Until(deadline))
	return c.UsageServiceClient.LinkOrganization(ctx, req)
}

type linkDispatchStub struct {
	accountsv1connect.UsageServiceClient
	link func(*connect.Request[accountsv1.LinkOrganizationRequest]) (*connect.Response[accountsv1.LinkOrganizationResponse], error)
}

func (s linkDispatchStub) LinkOrganization(_ context.Context, req *connect.Request[accountsv1.LinkOrganizationRequest]) (*connect.Response[accountsv1.LinkOrganizationResponse], error) {
	return s.link(req)
}

func TestLinkDispatchClientRecordsBothCalls(t *testing.T) {
	t.Parallel()

	firstDeadline := time.Now().Add(time.Minute)
	firstCtx, cancelFirst := context.WithDeadline(t.Context(), firstDeadline)
	defer cancelFirst()
	secondCtx, cancelSecond := context.WithDeadline(t.Context(), firstDeadline.Add(time.Minute))
	defer cancelSecond()

	firstReq := connect.NewRequest(&accountsv1.LinkOrganizationRequest{})
	secondReq := connect.NewRequest(&accountsv1.LinkOrganizationRequest{})
	wantResponse := connect.NewResponse(&accountsv1.LinkOrganizationResponse{})
	wantErr := errors.New("delegate rejected the second call")
	var forwarded []*connect.Request[accountsv1.LinkOrganizationRequest]
	stub := linkDispatchStub{link: func(req *connect.Request[accountsv1.LinkOrganizationRequest]) (*connect.Response[accountsv1.LinkOrganizationResponse], error) {
		forwarded = append(forwarded, req)
		if req == firstReq {
			return wantResponse, nil
		}
		return nil, wantErr
	}}

	var recorder linkDispatchRecorder
	client := linkDispatchClient{UsageServiceClient: stub, recorder: &recorder}
	var firstResp, secondResp *connect.Response[accountsv1.LinkOrganizationResponse]
	var firstErr, secondErr error
	done := make(chan struct{})
	before := time.Until(firstDeadline)
	go func() {
		defer close(done)
		firstResp, firstErr = client.LinkOrganization(firstCtx, firstReq)
		secondResp, secondErr = client.LinkOrganization(secondCtx, secondReq)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("two link dispatches did not return before reading the recorder")
	}
	after := time.Until(firstDeadline)

	if got := len(forwarded); got != 2 {
		t.Fatalf("forwarded calls = %d, want 2", got)
	}
	if forwarded[0] != firstReq || forwarded[1] != secondReq {
		t.Error("delegate did not receive the original requests in order")
	}
	if firstResp != wantResponse || firstErr != nil {
		t.Errorf("first dispatch = %v, %v, want %v, nil", firstResp, firstErr, wantResponse)
	}
	if secondResp != nil || secondErr != wantErr {
		t.Errorf("second dispatch = %v, %v, want nil, %v", secondResp, secondErr, wantErr)
	}
	observation := recorder.snapshot()
	if observation.calls != 2 {
		t.Fatalf("recorded calls = %d, want 2", observation.calls)
	}
	if budget := observation.firstBudget; budget < after || budget > before {
		t.Errorf("first recorded budget = %v, want [%v, %v]", budget, after, before)
	}
}

func TestBillingLinkDispatchAfterOwnershipLookup(t *testing.T) {
	t.Parallel()
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
				lookupStart: func(context.Context) {
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
			var recorder linkDispatchRecorder
			f = newLinkAttemptFixture(t, "dispatch-owner", func(o *app.Options) {
				testDB = o.DB
				o.DB = linkDispatchDB(t, testDB, trace)
				o.UsageClient = linkDispatchClient{UsageServiceClient: o.UsageClient, recorder: &recorder}
			})

			wantCalls := 0
			if tc.wantRemainingBudget > 0 {
				wantCalls = 1
				gate := f.fake.Hold(accountstest.MethodLinkOrganization)
				t.Cleanup(gate.Release)
			}
			err := f.link(f.org.OwnerClient, billingAccountA)
			observation := recorder.snapshot()
			if observation.calls != wantCalls {
				t.Errorf("recorded Accounts link calls = %d, want %d", observation.calls, wantCalls)
			}
			if calls := f.fake.Calls(accountstest.MethodLinkOrganization); calls != wantCalls {
				t.Errorf("fake Accounts link calls = %d, want %d", calls, wantCalls)
			}
			if testenv.Code(err) != tc.wantCode {
				t.Fatalf("link = %v, want %v", err, tc.wantCode)
			}
			if logged := strings.Contains(f.env.Logs.String(), "billing_link_attempt_expired_before_dispatch"); logged != tc.wantExpiry {
				t.Errorf("expiration logged = %t, want %t", logged, tc.wantExpiry)
			}
			if observation.calls == 1 && wantCalls == 1 {
				if budget := observation.firstBudget; budget <= 0 || budget > tc.wantRemainingBudget {
					t.Errorf("Accounts call budget = %v, want (0, %v]", budget, tc.wantRemainingBudget)
				}
			}
			if wantCalls == 1 {
				f.wantRows(t, 1, 1)
			} else {
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

func linkDispatchDB(t *testing.T, database *db.DB, trace linkDispatchTrace) *db.DB {
	t.Helper()
	cfg := database.Pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &db.DB{Pool: pool}
}
