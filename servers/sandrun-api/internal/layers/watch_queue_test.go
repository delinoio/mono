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
	"github.com/jackc/pgx/v5/pgconn"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/app"
	"github.com/delinoio/mono/servers/sandrun-api/internal/layers"
	"github.com/delinoio/mono/servers/sandrun-api/internal/lease"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient/ownsandfake"
	"github.com/delinoio/mono/servers/sandrun-api/internal/testenv"
	"github.com/delinoio/mono/servers/sandrun-api/internal/work"
)

func TestWatchQueueRequestIteration(t *testing.T) {
	for _, tc := range []struct {
		name         string
		failSnapshot int
		failPoll     bool
	}{
		{name: "snapshot fails before first row", failSnapshot: 98},
		{name: "snapshot fails after first row", failSnapshot: 100},
		{name: "poll fails after first changed row", failPoll: true},
		{name: "successful snapshot and poll"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			env := testenv.New(t, testenv.WithLayers(ownsandfake.New(), &layers.Layers{}), testenv.WithExtension(func(r *app.Registry) error {
				// Leave queue rows under the test's control while serving real RPCs.
				_, err := r.Service.DB.Pool.Exec(ctx, "INSERT INTO instance_leases VALUES ($1, 'test', 'infinity')", lease.Name)
				return err
			}))
			org := env.NewOrg("watch-owner")
			if _, err := env.DB.Pool.Exec(ctx, watchQueueFaultSchema); err != nil {
				t.Fatal(err)
			}

			seedWatchQueue(t, ctx, env, org.ID, 98, 100, 99, tc.failSnapshot, tc.failSnapshot != 0)
			watch, err := env.User(org.Member).Work.WatchQueue(ctx, connect.NewRequest(&sandrunv1.WatchQueueRequest{OrganizationId: org.ID}))
			if err != nil {
				t.Fatal(err)
			}
			if tc.failSnapshot == 0 {
				requireWatchQueueRows(t, watch, 98, 99, 100)
				if !watch.Receive() || watch.Msg().GetSnapshotComplete() == nil {
					t.Fatalf("watch did not complete the snapshot: %v", watch.Err())
				}

				var failSeq int
				if tc.failPoll {
					failSeq = 102
				}
				seedWatchQueue(t, ctx, env, org.ID, 101, 102, 1203, failSeq, tc.failPoll)
				if !tc.failPoll {
					requireWatchQueueRows(t, watch, 101, 102, 1203)
					return
				}
			}

			if watch.Receive() {
				t.Fatalf("watch sent an event from a failed batch: %v", watch.Msg())
			}
			wantCode(t, "queue read failure", watch.Err(), connect.CodeInternal)
			// The poll transaction is read-only. Give the marked runner a
			// distinct read error so it cannot mask the request iteration error.
			logs := env.Logs.String()
			if !strings.Contains(logs, "injected request row read failure") ||
				strings.Contains(logs, "injected runner row read failure") {
				t.Fatal("watch did not report the request iteration failure")
			}
			// Verify the fixture delivered the intended prefix to pgx before the error.
			const snapshotFilter = "state = ANY($2)"
			switch {
			case tc.failPoll:
				requireWatchRequestReadFailure(t, ctx, env, org.ID, "change_seq > $2", int64(100), 101)
			case tc.failSnapshot == 98:
				requireWatchRequestReadFailure(t, ctx, env, org.ID, snapshotFilter, work.OpenRequestStates())
			default:
				requireWatchRequestReadFailure(t, ctx, env, org.ID, snapshotFilter, work.OpenRequestStates(), 98)
			}
		})
	}
}

func requireWatchQueueRows(t *testing.T, watch *connect.ServerStreamForClient[sandrunv1.WatchQueueResponse], seqs ...int) {
	t.Helper()
	for _, seq := range seqs {
		if !watch.Receive() {
			t.Fatalf("watch ended before row %d: %v", seq, watch.Err())
		}
		want := watchQueueID(seq)
		msg := watch.Msg()
		if msg.GetRequest().GetRequestId() != want && msg.GetRunner().GetRunnerId() != want {
			t.Fatalf("expected watch row %d, got %v", seq, msg)
		}
	}
}

func requireWatchRequestReadFailure(t *testing.T, ctx context.Context, env *testenv.Env, orgID, filter string, arg any, seqs ...int) {
	t.Helper()
	rows, err := env.DB.Pool.Query(ctx, "SELECT "+work.RequestColumns+" FROM requests WHERE organization_id = $1 AND "+filter+" ORDER BY change_seq", orgID, arg)
	if err != nil {
		t.Fatalf("request query failed before iteration: %v", err)
	}
	defer rows.Close()
	for _, seq := range seqs {
		if !rows.Next() {
			t.Fatalf("request iterator ended before row %d: %v", seq, rows.Err())
		}
		r, err := work.ScanRequest(rows)
		if err != nil || r.ChangeSeq != int64(seq) {
			t.Fatalf("expected request row %d, got %v (%v)", seq, r, err)
		}
	}
	if rows.Next() {
		t.Fatal("request iterator returned an unexpected row")
	}
	var pgErr *pgconn.PgError
	if err := rows.Err(); !errors.As(err, &pgErr) || pgErr.Message != "injected request row read failure" {
		t.Fatalf("expected the injected iteration error, got %v", err)
	}
}

func watchQueueID(seq int) string {
	return fmt.Sprintf("018f0000-0000-7000-8000-%012d", seq)
}

func seedWatchQueue(t *testing.T, ctx context.Context, env *testenv.Env, orgID string, first, second, runner, failSeq int, markRunner bool) {
	t.Helper()
	// Commit all rows together so a poll cannot see only part of the fixture.
	_, err := env.DB.Pool.Exec(ctx, `
		WITH inserted_requests AS (
			INSERT INTO watch_requests (request_id, organization_id, runner_profile_id, kind, source, source_key, state, priority,
				queue_time, queue_deadline, raw_label, create_time, update_time, change_seq, watch_xact_id)
			SELECT req.id, $1, $2, 3, 1, req.id::text, 1, 2, now(), now() + interval '1 hour',
				CASE WHEN req.seq = $9 THEN 'fail' ELSE '' END, now(), now(), req.seq, pg_current_xact_id()
			FROM (VALUES ($3::uuid, $4::bigint), ($5::uuid, $6::bigint)) AS req(id, seq)
		)
		INSERT INTO watch_runners (runner_id, organization_id, runner_profile_id, kind, guest_os, state,
			idempotency_key, create_request, raw_device_id, create_time, update_time, change_seq, watch_xact_id)
		VALUES ($7, $1, $2, 3, 1, 5, $7, '', CASE WHEN $10 THEN 'mark' ELSE '' END, now(), now(), $8, pg_current_xact_id())
	`, orgID, watchQueueID(1), watchQueueID(first), first, watchQueueID(second), second, watchQueueID(runner), runner, failSeq, markRunner)
	if err != nil {
		t.Fatal(err)
	}
}

const watchQueueFaultSchema = `
	ALTER TABLE requests RENAME TO watch_requests;
	ALTER TABLE watch_requests RENAME COLUMN label TO raw_label;
	ALTER TABLE watch_requests DISABLE TRIGGER requests_touch;
	ALTER TABLE runners RENAME TO watch_runners;
	ALTER TABLE watch_runners RENAME COLUMN ownsand_device_id TO raw_device_id;
	ALTER TABLE watch_runners DISABLE TRIGGER runners_touch;

	CREATE FUNCTION watch_request_read(label text) RETURNS text STABLE COST 1000 LANGUAGE plpgsql AS $$
	BEGIN
		IF label = 'fail' THEN
			RAISE EXCEPTION 'injected request row read failure';
		END IF;
		RETURN label;
	END $$;
	-- Keep the function expensive so Postgres can order the rows before evaluating the label.
	CREATE VIEW requests AS SELECT *, watch_request_read(raw_label) AS label FROM watch_requests;

	CREATE FUNCTION watch_runner_read(device_id text) RETURNS text STABLE LANGUAGE plpgsql AS $$
	BEGIN
		IF device_id = 'mark' THEN
			RAISE EXCEPTION 'injected runner row read failure';
		END IF;
		RETURN device_id;
	END $$;
	CREATE VIEW runners AS SELECT *, watch_runner_read(raw_device_id) AS ownsand_device_id FROM watch_runners;
`
