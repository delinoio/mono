// SPDX-License-Identifier: FSL-1.1-ALv2

package billing_test

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/protos/delino/accounts/v1/accountsv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/billing"
)

func TestPendingUnlinkNullableBatch(t *testing.T) {
	t0 := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	f := &statusFixture{fixture: newFixture(t, t0)}
	ctx := t.Context()
	const unknownAccounts = 100
	orgID := func(i int) string { return fmt.Sprintf("0190f3a4-0000-7000-8000-%012d", i) }
	first := orgID(1)
	known := orgID(unknownAccounts + 1)

	// The queue can outlive the organizations. The known-account row is
	// due, but its deadline comes after a full batch of NULL-account rows.
	_, err := f.db.Pool.Exec(ctx, `INSERT INTO billing_pending_unlinks
		(organization_id, billing_account_id, create_time, next_attempt_time)
		SELECT ('0190f3a4-0000-7000-8000-' || lpad(i::text, 12, '0'))::uuid,
			CASE WHEN i = $1 + 1 THEN $2::uuid END,
			$3::timestamptz - interval '1 hour',
			$3::timestamptz - interval '1 hour' + i * interval '1 second'
		FROM generate_series(1, $1 + 1) AS i`, unknownAccounts, accountA, t0)
	if err != nil {
		t.Fatal(err)
	}

	calls := make(chan string, unknownAccounts+2)
	var failed atomic.Bool
	handler := connect.NewUnaryHandler(accountsv1connect.UsageServiceUnlinkOrganizationProcedure,
		func(_ context.Context, req *connect.Request[accountsv1.UnlinkOrganizationRequest]) (*connect.Response[accountsv1.UnlinkOrganizationResponse], error) {
			org := req.Msg.GetOrganizationId()
			if !proto.Equal(req.Msg, &accountsv1.UnlinkOrganizationRequest{Service: billing.ServiceID, OrganizationId: org}) {
				t.Errorf("unexpected unlink request: %v", req.Msg)
			}
			select {
			case calls <- org:
			default:
				return nil, connect.NewError(connect.CodeInternal, errors.New("unexpected additional unlink"))
			}
			if org == first && !failed.Swap(true) {
				return nil, connect.NewError(connect.CodeUnavailable, errors.New("temporary failure"))
			}
			return connect.NewResponse(&accountsv1.UnlinkOrganizationResponse{}), nil
		})
	server := httptest.NewServer(handler)
	defer server.Close()
	worker := billing.NewStatuses(billing.StatusOptions{
		DB: f.db, Logger: f.logger, Now: f.clock.Now,
		Client: accountsv1connect.NewUsageServiceClient(server.Client(), server.URL),
	})
	retry := func(wantCalls int) {
		t.Helper()
		if err := worker.RetryPendingUnlinks(ctx); err != nil {
			t.Fatal(err)
		}
		if len(calls) != wantCalls {
			t.Fatalf("retry made %d unlink calls, want %d", len(calls), wantCalls)
		}
	}

	retry(unknownAccounts)
	for i := 1; i <= unknownAccounts; i++ {
		if got := <-calls; got != orgID(i) {
			t.Fatalf("unlink call %d sent to %s, want %s", i, got, orgID(i))
		}
	}
	if a, next, ok := f.pendingUnlink(t, first); !ok || a != 1 || !next.Equal(t0.Add(time.Second)) {
		t.Fatalf("failed NULL-account row has attempts=%d next=%v present=%t", a, next, ok)
	}

	retry(1)
	if got := <-calls; got != known {
		t.Fatalf("second pass unlinked %s, want %s", got, known)
	}
	if _, _, ok := f.pendingUnlink(t, known); ok {
		t.Fatal("the known-account row stayed after success")
	}
	f.clock.Advance(time.Second)
	retry(1)
	if got := <-calls; got != first {
		t.Fatalf("retry after backoff unlinked %s, want %s", got, first)
	}
	var remaining int
	if err := f.db.Pool.QueryRow(ctx, "SELECT count(*) FROM billing_pending_unlinks").Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("pending rows = %d, error = %v", remaining, err)
	}
}
