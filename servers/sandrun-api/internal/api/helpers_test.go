// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
	"github.com/delinoio/mono/servers/sandrun-api/internal/app"
	"github.com/delinoio/mono/servers/sandrun-api/internal/authz"
	"github.com/delinoio/mono/servers/sandrun-api/internal/testenv"
)

// fakeDevSessions stands in for the later dev session layer: it is mounted
// through the Registry like a real layer and authorizes with
// ActionUseDevSessions, so the tests can check that personal API keys work
// there and nowhere else.
type fakeDevSessions struct {
	sandrunv1connect.UnimplementedDevSessionServiceHandler
	svc *api.Service
}

func (f *fakeDevSessions) ListDevSessionProfiles(ctx context.Context, req *connect.Request[sandrunv1.ListDevSessionProfilesRequest]) (*connect.Response[sandrunv1.ListDevSessionProfilesResponse], error) {
	caller, err := f.svc.Authorize(ctx, f.svc.DB.Pool, req.Msg.GetOrganizationId(), authz.ActionUseDevSessions)
	if err != nil {
		return nil, err
	}
	f.svc.RecordAuthorizedAPIKeyUse(ctx, caller)
	return connect.NewResponse(&sandrunv1.ListDevSessionProfilesResponse{}), nil
}

func withFakeDevSessions() testenv.Option {
	return testenv.WithExtension(func(r *app.Registry) error {
		r.Handle(sandrunv1connect.NewDevSessionServiceHandler(&fakeDevSessions{svc: r.Service}, r.HandlerOptions()...))
		return nil
	})
}

func listProfiles(c *testenv.Clients, orgID string) error {
	_, err := c.DevSession.ListDevSessionProfiles(context.Background(), connect.NewRequest(&sandrunv1.ListDevSessionProfilesRequest{OrganizationId: orgID}))
	return err
}

func wantCode(t *testing.T, what string, err error, want connect.Code) {
	t.Helper()
	if got := testenv.Code(err); got != want {
		t.Fatalf("%s: code %v (%v), want %v", what, got, err, want)
	}
}

// auditActions returns the actions of the organization's audit events,
// newest first.
func auditActions(t *testing.T, env *testenv.Env, orgID string) []sandrunv1.AuditAction {
	t.Helper()
	rows, err := env.DB.Pool.Query(context.Background(), "SELECT action FROM audit_events WHERE organization_id = $1 ORDER BY audit_event_id DESC", orgID)
	if err != nil {
		t.Fatalf("read audit actions: %v", err)
	}
	defer rows.Close()

	actions, err := collectAuditActions(rows)
	if err != nil {
		t.Fatalf("read audit actions: %v", err)
	}
	return actions
}

func collectAuditActions(rows pgx.Rows) ([]sandrunv1.AuditAction, error) {
	var out []sandrunv1.AuditAction
	for rows.Next() {
		var a int16
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, sandrunv1.AuditAction(a))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

type auditActionRow struct {
	action sandrunv1.AuditAction
	err    error
}

type auditActionRows struct {
	pgx.Rows
	rows         []auditActionRow
	next         int
	exhausted    bool
	iterationErr error
}

func (r *auditActionRows) Next() bool {
	if r.next == len(r.rows) {
		r.exhausted = true
		return false
	}
	r.next++
	return true
}

func (r *auditActionRows) Scan(dest ...any) error {
	row := r.rows[r.next-1]
	if row.err != nil {
		return row.err
	}
	*dest[0].(*int16) = int16(row.action)
	return nil
}

func (r *auditActionRows) Err() error {
	if r.exhausted {
		return r.iterationErr
	}
	return nil
}

func TestCollectAuditActions(t *testing.T) {
	t.Parallel()
	iterationErr := errors.New("audit row iteration failed")
	scanErr := errors.New("audit row scan failed")
	created := auditActionRow{action: sandrunv1.AuditAction_AUDIT_ACTION_ORGANIZATION_CREATED}
	joined := auditActionRow{action: sandrunv1.AuditAction_AUDIT_ACTION_MEMBER_JOINED}

	for _, tc := range []struct {
		name         string
		rows         []auditActionRow
		iterationErr error
		want         []sandrunv1.AuditAction
		wantErr      error
	}{
		{name: "empty"},
		{name: "preserve query order", rows: []auditActionRow{joined, created}, want: []sandrunv1.AuditAction{joined.action, created.action}},
		{name: "iteration fails before any row", iterationErr: iterationErr, wantErr: iterationErr},
		{name: "iteration fails after a row", rows: []auditActionRow{created}, iterationErr: iterationErr, wantErr: iterationErr},
		{name: "scan fails after a row", rows: []auditActionRow{created, {err: scanErr}}, wantErr: scanErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := collectAuditActions(&auditActionRows{rows: tc.rows, iterationErr: tc.iterationErr})
			if err != tc.wantErr || !slices.Equal(got, tc.want) {
				t.Fatalf("collectAuditActions = %v, %v, want %v, %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func countAction(actions []sandrunv1.AuditAction, a sandrunv1.AuditAction) int {
	n := 0
	for _, x := range actions {
		if x == a {
			n++
		}
	}
	return n
}

// holdTx runs sql in a transaction that stays open until the returned
// function commits it, so a test can make an RPC wait on its row locks.
func holdTx(t *testing.T, env *testenv.Env, sql string, args ...any) (commit func()) {
	t.Helper()
	ctx := context.Background()
	tx, err := env.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	done := false
	t.Cleanup(func() {
		if !done {
			_ = tx.Rollback(ctx)
		}
	})
	return func() {
		t.Helper()
		done = true
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

// waitForLockWaiter waits until a session of the test database waits on a
// lock.
func waitForLockWaiter(t *testing.T, env *testenv.Env) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := env.DB.Pool.QueryRow(context.Background(),
			"SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no session waits on a lock")
}
