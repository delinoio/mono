// SPDX-License-Identifier: FSL-1.1-ALv2

package jobs_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/delinoio/mono/go/uuidv7"
	"github.com/delinoio/mono/servers/sandrun-api/internal/db/dbtest"
	"github.com/delinoio/mono/servers/sandrun-api/internal/jobs"
)

func TestDailyContinuesAfterFailure(t *testing.T) {
	var logs bytes.Buffer
	var ran []string
	d := &jobs.Daily{Logger: slog.New(slog.NewJSONHandler(&logs, nil)), Jobs: []jobs.Job{
		{Name: "fails", Run: func(context.Context, time.Time) (int64, error) {
			ran = append(ran, "fails")
			return 0, errors.New("boom")
		}},
		{Name: "works", Run: func(context.Context, time.Time) (int64, error) { ran = append(ran, "works"); return 3, nil }},
	}}
	d.RunOnce(context.Background(), time.Now())
	if strings.Join(ran, ",") != "fails,works" {
		t.Fatalf("ran %v", ran)
	}
	if !strings.Contains(logs.String(), `"daily_job_failed"`) || !strings.Contains(logs.String(), `"rows":3`) {
		t.Fatalf("logs: %s", logs.String())
	}
}

func TestAuditRetention(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	now := time.Now().UTC()
	insert := func(age time.Duration) string {
		id, _ := uuidv7.NewString()
		org, _ := uuidv7.NewString()
		if _, err := d.Pool.Exec(ctx, `INSERT INTO audit_events (audit_event_id, organization_id, action, actor_user_id, target_kind, target_id, subject_user_id, rpc_id, create_time)
			VALUES ($1, $2, 1, 'u', 1, $3, '', 'r', $4)`, id, org, org, now.Add(-age)); err != nil {
			t.Fatal(err)
		}
		return id
	}
	old := insert(91 * 24 * time.Hour)
	recent := insert(89 * 24 * time.Hour)
	n, err := jobs.AuditRetention(d).Run(ctx, now)
	if err != nil || n != 1 {
		t.Fatalf("deleted %d, %v", n, err)
	}
	var left []string
	rows, _ := d.Pool.Query(ctx, "SELECT audit_event_id::text FROM audit_events")
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		left = append(left, id)
	}
	rows.Close()
	if len(left) != 1 || left[0] != recent || left[0] == old {
		t.Fatalf("left %v", left)
	}
}
