// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/layers"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient/ownsandfake"
	"github.com/delinoio/mono/servers/sandrun-api/internal/testenv"
)

type world struct {
	env  *testenv.Env
	fake *ownsandfake.Fake
	l    *layers.Layers
	org  testenv.Org
}

func newWorld(t *testing.T, opts ...testenv.Option) *world {
	t.Helper()
	w := &world{fake: ownsandfake.New(), l: &layers.Layers{}}
	w.env = testenv.New(t, append([]testenv.Option{testenv.WithLayers(w.fake, w.l)}, opts...)...)
	w.org = w.env.NewOrg("alice")
	return w
}

func ownsandKey(c byte) string { return "osk_" + strings.Repeat(string(c), 43) }

func wantCode(t *testing.T, what string, err error, want connect.Code) {
	t.Helper()
	if got := testenv.Code(err); got != want {
		t.Fatalf("%s: code %v (%v), want %v", what, got, err, want)
	}
}

func (w *world) setLink(user, ownsandOrg, key string) (*sandrunv1.OwnSandLink, error) {
	resp, err := w.env.User(user).OwnSandLink.SetOwnSandLink(context.Background(), connect.NewRequest(&sandrunv1.SetOwnSandLinkRequest{
		OrganizationId: w.org.ID, OwnsandOrganizationId: ownsandOrg, OwnsandApiKey: key}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetLink(), nil
}

func countAudit(t *testing.T, env *testenv.Env, orgID string, action sandrunv1.AuditAction) int {
	t.Helper()
	var n int
	if err := env.DB.Pool.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE organization_id = $1 AND action = $2", orgID, int16(action)).Scan(&n); err != nil {
		t.Fatal(err)
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
