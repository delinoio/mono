// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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

const lockWaitTimeout = 10 * time.Second

type heldTx struct {
	pid    int32
	commit func()
}

// holdTx runs sql and holds its locks until commit or test cleanup.
func holdTx(t *testing.T, env *testenv.Env, sql string, args ...any) heldTx {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), lockWaitTimeout)
	defer cancel()
	tx, err := env.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), lockWaitTimeout)
		defer cancel()
		_ = tx.Rollback(ctx)
	})
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
	return heldTx{
		pid: int32(tx.Conn().PgConn().PID()),
		commit: func() {
			t.Helper()
			ctx, cancel := context.WithTimeout(t.Context(), lockWaitTimeout)
			defer cancel()
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
		},
	}
}

type lockWait struct {
	blockerPID int32
	waiterPID  int32
	query      string
	// A second row-lock waiter can queue behind the first waiter.
	earlierWaiterPID int32
}

func waitForLockWaiter(t *testing.T, env *testenv.Env, wait lockWait) int32 {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), lockWaitTimeout)
	defer cancel()
	pid, err := observeLockWaiter(ctx, env.DB.Pool, wait)
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// observeLockWaiter matches the fixture holder and the expected waiter.
// The same context bounds pool acquisition, query execution, and polling.
func observeLockWaiter(ctx context.Context, pool *pgxpool.Pool, wait lockWait) (int32, error) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var pid int32
		err := pool.QueryRow(ctx, `
			SELECT pid FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'
			  AND ($2::int = 0 OR pid = $2)
			  AND strpos(query, $3) > 0
			  AND pid <> $4
			  AND pg_blocking_pids(pid) && ARRAY[$1::int, NULLIF($4::int, 0)]
			LIMIT 1`, wait.blockerPID, wait.waiterPID, wait.query, wait.earlierWaiterPID).Scan(&pid)
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = ctxErr
		}
		if err == nil {
			return pid, nil
		}
		if errors.Is(err, pgx.ErrNoRows) {
			select {
			case <-ticker.C:
				continue
			case <-ctx.Done():
				err = ctx.Err()
			}
		}
		return 0, fmt.Errorf("wait for PostgreSQL lock held by PID %d (waiter PID %d, SQL fragment %q, earlier waiter PID %d): %w",
			wait.blockerPID, wait.waiterPID, wait.query, wait.earlierWaiterPID, err)
	}
}
