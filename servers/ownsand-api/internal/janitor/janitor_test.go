// SPDX-License-Identifier: FSL-1.1-ALv2

package janitor_test

import (
	"context"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/ownsand-api/internal/db/dbtest"
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
	janitor.RunOnce(ctx, d, testenv.Discard(), now)
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
}
