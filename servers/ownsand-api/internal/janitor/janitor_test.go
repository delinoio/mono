// SPDX-License-Identifier: FSL-1.1-ALv2

package janitor_test

import (
	"context"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/ownsand-api/internal/db/dbtest"
	"github.com/delinoio/mono/servers/ownsand-api/internal/events"
	"github.com/delinoio/mono/servers/ownsand-api/internal/janitor"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

func TestRetentionJobs(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	now := time.Now().UTC()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := d.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("INSERT INTO consumed_nonces VALUES ('\\x01', $1), ('\\x02', $2)", now.Add(-time.Second), now.Add(time.Minute))
	exec(`INSERT INTO audit_events (audit_event_id, organization_id, action, actor_kind, actor_id, target_kind, target_id, request_id, create_time) VALUES
		('01900000-0000-7000-8000-000000000001', '01900000-0000-7000-8000-000000000000', 1, 1, 'a', 1, 'x', 'r', $1),
		('01900000-0000-7000-8000-000000000002', '01900000-0000-7000-8000-000000000000', 1, 1, 'a', 1, 'x', 'r', $2)`,
		now.Add(-91*24*time.Hour), now.Add(-89*24*time.Hour))
	exec(`INSERT INTO devices (device_id, organization_id, public_key, name, host_os, arch, agent_version, state, create_time, tombstone_expire_time) VALUES
		('01900000-0000-7000-8000-000000000003', '01900000-0000-7000-8000-000000000000', '\x00', 'h', 1, 1, '1', 3, $1, $1),
		('01900000-0000-7000-8000-000000000004', '01900000-0000-7000-8000-000000000000', '\x00', 'h', 1, 1, '1', 3, $1, $2)`,
		now.Add(-time.Hour), now.Add(time.Hour))
	// Reported outbox rows go 35 days after the report; unreported rows of
	// any age stay.
	exec(`INSERT INTO usage_outbox (organization_id, meter, window_start, quantity, amount_usd_micros, seal_time, next_attempt_time, report_time) VALUES
		('01900000-0000-7000-8000-000000000000', 1, $1, 1, 1, $1, $1, $1),
		('01900000-0000-7000-8000-000000000000', 2, $1, 1, 1, $1, $1, $2),
		('01900000-0000-7000-8000-000000000000', 3, $1, 1, 1, $1, $1, NULL)`,
		now.Add(-36*24*time.Hour), now.Add(-34*24*time.Hour))
	exec("INSERT INTO usage_storage_samples VALUES ($1, $1), ($2, $2)", now.Add(-25*time.Hour), now.Add(-23*time.Hour))
	janitor.RunOnce(ctx, d, testenv.Discard(), now, nil)
	count := func(sql string) int {
		var n int
		if err := d.Pool.QueryRow(ctx, sql).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count("SELECT count(*) FROM consumed_nonces"); n != 1 {
		t.Fatalf("nonces = %d", n)
	}
	if n := count("SELECT count(*) FROM audit_events"); n != 1 {
		t.Fatalf("audit events = %d", n)
	}
	if n := count("SELECT count(*) FROM devices"); n != 1 {
		t.Fatalf("tombstones = %d", n)
	}
	if n := count("SELECT count(*) FROM usage_outbox WHERE meter IN (2, 3)"); n != 2 || count("SELECT count(*) FROM usage_outbox") != 2 {
		t.Fatalf("usage outbox = %d", n)
	}
	if n := count("SELECT count(*) FROM usage_storage_samples"); n != 1 {
		t.Fatalf("storage samples = %d", n)
	}
}

// TestControlEventsBoundByProcessedCursor checks that the janitor deletes a
// control event only when it is older than the retention of 1 hour and the
// lease holder has processed it (delivery ID up to its cursor).
func TestControlEventsBoundByProcessedCursor(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	now := time.Now().UTC()
	// Six events in delivery order; the age is set after the insert because
	// the column defaults to the insert time.
	ages := []time.Duration{3 * time.Hour, 2 * time.Hour, 90 * time.Minute, 2 * time.Hour, 30 * time.Minute, 10 * time.Minute}
	for range ages {
		if err := events.Write(ctx, d.Pool, events.Event{Kind: events.KindDeviceConfigChanged}); err != nil {
			t.Fatal(err)
		}
	}
	all, err := events.After(ctx, d.Pool, 0, 100)
	if err != nil || len(all) != len(ages) {
		t.Fatalf("events = %d, %v", len(all), err)
	}
	for i, e := range all {
		if _, err := d.Pool.Exec(ctx, "UPDATE control_events SET create_time = $2 WHERE delivery_id = $1", e.ID, now.Add(-ages[i])); err != nil {
			t.Fatal(err)
		}
	}
	remaining := func() []int64 {
		t.Helper()
		got, err := events.After(ctx, d.Pool, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		var ids []int64
		for _, e := range got {
			ids = append(ids, e.ID)
		}
		return ids
	}
	ids := func(from, to int) []int64 {
		var out []int64
		for _, e := range all[from:to] {
			out = append(out, e.ID)
		}
		return out
	}
	equal := func(a, b []int64) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}

	// Without a cursor (a nil cursor, no lease or an unset cursor) the job
	// does not run, however old the events are.
	janitor.RunOnce(ctx, d, testenv.Discard(), now, nil)
	janitor.RunOnce(ctx, d, testenv.Discard(), now, func() (int64, bool) { return 0, false })
	if got := remaining(); !equal(got, ids(0, 6)) {
		t.Fatalf("events without a cursor = %v, want all of %v", got, ids(0, 6))
	}

	// The holder processed the first three events, all older than the
	// retention: they go. The fourth is older than the retention too, but
	// the holder has not processed it, so it stays.
	janitor.RunOnce(ctx, d, testenv.Discard(), now, func() (int64, bool) { return all[2].ID, true })
	if got := remaining(); !equal(got, ids(3, 6)) {
		t.Fatalf("events after the cursor at the third event = %v, want %v", got, ids(3, 6))
	}

	// With the cursor at the newest event, the fourth event goes, and the
	// processed events that are younger than the retention stay.
	janitor.RunOnce(ctx, d, testenv.Discard(), now, func() (int64, bool) { return all[5].ID, true })
	if got := remaining(); !equal(got, ids(4, 6)) {
		t.Fatalf("events with the cursor at the newest event = %v, want %v", got, ids(4, 6))
	}
	// Processed events still expire after the retention.
	janitor.RunOnce(ctx, d, testenv.Discard(), now.Add(2*time.Hour), func() (int64, bool) { return all[5].ID, true })
	if got := remaining(); len(got) != 0 {
		t.Fatalf("events after the retention = %v, want none", got)
	}
}

// TestControlEventsWiredToLeaseHolderCursor runs the janitor with the cursor
// accessor of a running control plane: once the lease holder processed an
// event and the retention passed, the event goes.
func TestControlEventsWiredToLeaseHolderCursor(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	if err := events.Write(ctx, env.DB.Pool, events.Event{Kind: events.KindDeviceConfigChanged}); err != nil {
		t.Fatal(err)
	}
	newest, err := events.LatestID(ctx, env.DB.Pool)
	if err != nil || newest == 0 {
		t.Fatalf("newest event = %d, %v", newest, err)
	}
	cursor := env.App.Control.ProcessedCursor
	deadline := time.Now().Add(10 * time.Second)
	for {
		if id, ok := cursor(); ok && id >= newest {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the lease holder did not process the event")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The event is processed but younger than the retention.
	janitor.RunOnce(ctx, env.DB, testenv.Discard(), env.Clock.Now(), cursor)
	if got, _ := events.LatestID(ctx, env.DB.Pool); got != newest {
		t.Fatalf("a processed event younger than the retention was deleted (newest %d)", got)
	}
	janitor.RunOnce(ctx, env.DB, testenv.Discard(), env.Clock.Now().Add(2*time.Hour), cursor)
	var n int
	if err := env.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM control_events WHERE delivery_id = $1", newest).Scan(&n); err != nil || n != 0 {
		t.Fatalf("processed event after the retention: count %d, %v", n, err)
	}
}
