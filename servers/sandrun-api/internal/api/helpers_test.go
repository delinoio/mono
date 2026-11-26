// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

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
	if _, err := f.svc.Authorize(ctx, f.svc.DB.Pool, req.Msg.GetOrganizationId(), authz.ActionUseDevSessions); err != nil {
		return nil, err
	}
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
		t.Fatal(err)
	}
	defer rows.Close()
	var out []sandrunv1.AuditAction
	for rows.Next() {
		var a int16
		if err := rows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		out = append(out, sandrunv1.AuditAction(a))
	}
	return out
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
