// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

type deleteWaitRead struct {
	pid      uint32
	deadline time.Time
}

type deleteWaitTrace struct {
	sandboxID string
	blockRead int32
	reads     atomic.Int32
	started   chan deleteWaitRead
	unblock   chan struct{}
}

type deleteWaitTracer struct {
	active atomic.Pointer[deleteWaitTrace]
}

func (tr *deleteWaitTracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	trace := tr.active.Load()
	principal, _ := auth.PrincipalFrom(ctx)
	if trace == nil || !principal.IsUser() || !strings.HasSuffix(data.SQL, "FROM sandboxes s LEFT JOIN devices d ON d.device_id = s.device_id WHERE s.organization_id = $1 AND s.sandbox_id = $2") ||
		data.Args[1] != trace.sandboxID {
		return ctx
	}
	read := trace.reads.Add(1)
	deadline, _ := ctx.Deadline()
	trace.started <- deleteWaitRead{pid: conn.PgConn().PID(), deadline: deadline}
	if read == trace.blockRead {
		// Give the test time to acquire the lock before PostgreSQL runs the query.
		select {
		case <-trace.unblock:
		case <-ctx.Done():
		}
	}
	return ctx
}

func (*deleteWaitTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestDeleteSandboxWaitDeadline(t *testing.T) {
	const commandWait = 900 * time.Millisecond
	for _, scenario := range []struct {
		name      string
		blockRead int32
	}{
		{name: "initial_read", blockRead: 1},
		{name: "notification_refresh", blockRead: 2},
		{name: "between_reads"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := t.Context()
			tracer := &deleteWaitTracer{}
			env := testenv.New(t, func(o *app.Options) {
				o.Timing.CommandWait = commandWait
				cfg := o.DB.Pool.Config()
				cfg.ConnConfig.Tracer = tracer
				pool, err := pgxpool.NewWithConfig(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(pool.Close)
				o.DB = &db.DB{Pool: pool}
			})
			org := env.NewOrg("alice")
			_, agent := setupAgent(t, env, org, nil)
			client := org.OwnerClient
			created, err := client.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
			if err != nil {
				t.Fatal(err)
			}
			id := created.Msg.GetSandbox().GetSandboxId()
			const running = ownsandv1.SandboxState_SANDBOX_STATE_RUNNING
			waitState(t, client, org.ID, id, running)
			agent.SetOnCommand(func(agent *testenv.FakeAgent, cmd *ownsandv1.AgentCommand) *ownsandv1.AgentCommandResult {
				if cmd.GetDeleteSandbox() != nil {
					return nil
				}
				return agent.DefaultAnswer(cmd)
			})

			trace := &deleteWaitTrace{
				sandboxID: id, blockRead: scenario.blockRead,
				started: make(chan deleteWaitRead, 8), unblock: make(chan struct{}),
			}
			tracer.active.Store(trace)
			type result struct {
				response *connect.Response[ownsandv1.DeleteSandboxResponse]
				err      error
			}
			finished := make(chan result, 1)
			deleteCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			go func() {
				resp, err := client.Sandbox.DeleteSandbox(deleteCtx, connect.NewRequest(&ownsandv1.DeleteSandboxRequest{
					OrganizationId: org.ID, SandboxId: id,
				}))
				finished <- result{resp, err}
			}()
			nextRead := func() deleteWaitRead {
				t.Helper()
				select {
				case read := <-trace.started:
					return read
				case <-deleteCtx.Done():
					t.Fatal("delete did not start the expected state read")
					return deleteWaitRead{}
				}
			}
			first := nextRead()
			blocked := first
			if scenario.blockRead == 2 {
				// A nonterminal update wakes the waiter before the first poll.
				agent.SendUpdate(agent.SetSandbox(id, running, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED))
				blocked = nextRead()
			}

			var lock pgx.Tx
			if scenario.blockRead != 0 {
				lock, err = env.DB.Pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Rollback(context.Background())
				if _, err := lock.Exec(ctx, "LOCK TABLE sandboxes IN ACCESS EXCLUSIVE MODE"); err != nil {
					t.Fatal(err)
				}
				close(trace.unblock)
				testenv.Eventually(t, "delete state read blocked on a PostgreSQL lock", func() bool {
					var waiting bool
					err := env.DB.Pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1 AND wait_event_type = 'Lock')", blocked.pid).Scan(&waiting)
					if err != nil {
						t.Fatal(err)
					}
					return waiting
				})
			}

			select {
			case got := <-finished:
				if scenario.blockRead == 0 {
					if got.err != nil || got.response.Msg.GetSandbox().GetState() != ownsandv1.SandboxState_SANDBOX_STATE_DELETING {
						t.Fatalf("delete between reads = %v, %v", got.response, got.err)
					}
				} else if !connect.IsWireError(got.err) || connect.CodeOf(got.err) != connect.CodeDeadlineExceeded {
					t.Fatalf("blocked state read = %v, want server deadline_exceeded", got.err)
				}
			case <-time.After(2 * commandWait):
				t.Fatal("delete did not return within the command wait while the SQL lock was held")
			}
			if lock != nil {
				if err := lock.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if first.deadline.IsZero() || time.Until(first.deadline) > 0 || !blocked.deadline.Equal(first.deadline) {
				t.Fatalf("state read deadlines differ from the expired first-read deadline: first %v, last %v", first.deadline, blocked.deadline)
			}
			if want := max(1, scenario.blockRead); trace.reads.Load() != want {
				t.Fatalf("started %d state reads, want %d", trace.reads.Load(), want)
			}
			if count := env.App.Service.WatcherCount(); count != 0 {
				t.Fatalf("watchers after delete = %d", count)
			}
			var pending bool
			if err := env.DB.Pool.QueryRow(ctx, "SELECT delete_pending FROM sandboxes WHERE sandbox_id = $1", id).Scan(&pending); err != nil || !pending {
				t.Fatalf("delete_pending = %v, %v", pending, err)
			}
			t.Log("verified server response, shared state-read deadline, watcher cleanup, and committed delete")
		})
	}
}
