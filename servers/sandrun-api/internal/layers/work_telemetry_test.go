// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
	"github.com/delinoio/mono/servers/sandrun-api/internal/app"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient/ownsandfake"
)

func TestCancelRequestTelemetryAfterCommit(t *testing.T) {
	t.Parallel()
	w, reader := newWorkTelemetryWorld(t)
	owner := w.env.User(w.org.Owner)
	profileID := w.createProfile(t, devProfileProto("dev", 0, sandrunv1.Priority_PRIORITY_NORMAL))
	session := w.createSession(t, owner, profileID)
	id := session.GetDevSessionId()
	w.env.Clock.Advance(7 * time.Second)

	cancel := func() (*connect.Response[sandrunv1.CancelRequestResponse], error) {
		return owner.Work.CancelRequest(t.Context(), connect.NewRequest(&sandrunv1.CancelRequestRequest{
			OrganizationId: w.org.ID, RequestId: id,
		}))
	}
	const action = sandrunv1.AuditAction_AUDIT_ACTION_REQUEST_CANCELED
	const queued = sandrunv1.RequestState_REQUEST_STATE_QUEUED
	const canceled = sandrunv1.RequestState_REQUEST_STATE_CANCELED
	const userCanceled = sandrunv1.RequestEndReason_REQUEST_END_REASON_CANCELED_BY_USER
	endedLabels := workLabels("state", "CANCELED", "end_reason", "CANCELED_BY_USER")
	queueLabels := workLabels("priority", "NORMAL")
	assertSignals := func(wantCount uint64, wantWait float64) {
		t.Helper()
		if got := countAudit(t, w.env, w.org.ID, action); uint64(got) != wantCount {
			t.Fatalf("cancel audit count = %d, want %d", got, wantCount)
		}
		if got := transitionLogs(t, w, id, "request_state_changed", "QUEUED", "CANCELED", "CANCELED_BY_USER"); uint64(got) != wantCount {
			t.Fatalf("queued cancellation logs = %d, want %d", got, wantCount)
		}
		assertWorkMetric(t, reader, "sandrun_requests_ended", endedLabels, wantCount, 0)
		assertWorkMetric(t, reader, "sandrun_request_queue_wait_seconds", queueLabels, wantCount, wantWait)
	}

	execTelemetrySQL(t, w, fmt.Sprintf("ALTER TABLE audit_events ADD CONSTRAINT reject_cancel CHECK (action <> %d)", action))
	_, err := cancel()
	wantCode(t, "cancel with rejected audit insert", err, connect.CodeInternal)
	if got := w.session(t, id).GetState(); got != queued {
		t.Fatalf("request state after the rejected insert = %v, want QUEUED", got)
	}
	assertSignals(0, 0)
	execTelemetrySQL(t, w, "ALTER TABLE audit_events DROP CONSTRAINT reject_cancel")

	execTelemetrySQL(t, w, fmt.Sprintf(`
		CREATE FUNCTION reject_cancel_commit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'injected cancellation commit failure' USING ERRCODE = '23514';
		END;
		$$;
		CREATE CONSTRAINT TRIGGER reject_cancel_commit AFTER INSERT ON audit_events
			DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.action = %d)
			EXECUTE FUNCTION reject_cancel_commit();`, action))
	_, err = cancel()
	wantCode(t, "cancel with deferred audit failure", err, connect.CodeInternal)
	if got := w.session(t, id).GetState(); got != queued {
		t.Fatalf("request state after the rejected commit = %v, want QUEUED", got)
	}
	assertSignals(0, 0)
	execTelemetrySQL(t, w, "DROP TRIGGER reject_cancel_commit ON audit_events")

	result, err := cancel()
	if err != nil {
		t.Fatal(err)
	}
	if r := result.Msg.GetRequest(); r.GetState() != canceled || r.GetEndReason() != userCanceled {
		t.Fatalf("cancel retry returned %v", r)
	}
	if got := w.session(t, id).GetState(); got != canceled {
		t.Fatalf("persisted request state = %v, want CANCELED", got)
	}
	assertSignals(1, 7)
	t.Log("Statement and commit failures each left the request queued with zero audits, cancellation logs, ended events and queue-wait samples. The retry persisted CANCELED_BY_USER and emitted one of each.")
}

func TestRequestStartTelemetryAfterCommit(t *testing.T) {
	t.Parallel()
	w, reader := newWorkTelemetryWorld(t)
	profileID := w.createProfile(t, devProfileProto("dev", 0, sandrunv1.Priority_PRIORITY_NORMAL))
	id := w.createSession(t, w.env.User(w.org.Owner), profileID).GetDevSessionId()
	failCommit := rejectWorkTransitionCommit(t, w, "requests",
		int16(sandrunv1.RequestState_REQUEST_STATE_QUEUED), int16(sandrunv1.RequestState_REQUEST_STATE_STARTING))
	w.env.Clock.Advance(7 * time.Second)
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.CreateError = nil })
	waitForRejectedWorkCommit(t, w)
	if got := w.session(t, id).GetState(); got != sandrunv1.RequestState_REQUEST_STATE_QUEUED {
		t.Fatalf("request state after rollback = %v, want QUEUED", got)
	}
	assertWorkMetric(t, reader, "sandrun_request_queue_wait_seconds", workLabels("priority", "NORMAL"), 0, 0)
	if got := transitionLogs(t, w, id, "request_state_changed", "QUEUED", "STARTING", "NONE"); got != 0 {
		t.Fatalf("request start logs after rollback = %d", got)
	}

	failCommit()
	w.waitState(t, id, sandrunv1.RequestState_REQUEST_STATE_STARTING)
	eventually(t, "post-commit request start log", func() bool {
		return transitionLogs(t, w, id, "request_state_changed", "QUEUED", "STARTING", "NONE") == 1 &&
			workMetricCount(t, reader, "sandrun_request_queue_wait_seconds", workLabels("priority", "NORMAL")) == 1
	})
	assertWorkMetric(t, reader, "sandrun_request_queue_wait_seconds", workLabels("priority", "NORMAL"), 1, 7)
	t.Log("A rejected start commit left the request queued with zero start logs and samples. The committed retry produced one start log and a 7-second queue-wait sample.")
}

func TestRunnerReadinessTelemetryAfterCommit(t *testing.T) {
	t.Parallel()
	w, reader := newWorkTelemetryWorld(t)
	const preparing = sandrunv1.RunnerState_RUNNER_STATE_PREPARING
	const warm = sandrunv1.RunnerState_RUNNER_STATE_WARM
	failCommit := rejectWorkTransitionCommit(t, w, "runners", int16(preparing), int16(warm))
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.CreateError = nil })
	profileID := w.createProfile(t, devProfileProto("warm", 1, sandrunv1.Priority_PRIORITY_NORMAL))
	var sandboxID, runnerID string
	eventually(t, "warm reservation", func() bool {
		sandboxes := w.fake.Sandboxes(w.osOrg)
		if len(sandboxes) != 1 {
			return false
		}
		sandboxID = sandboxes[0].GetSandboxId()
		runnerID = sandboxes[0].GetLabels()["sandrun.runner-id"]
		return true
	})
	w.env.Clock.Advance(13 * time.Second)
	w.fake.SetRunning(w.osOrg, sandboxID)
	waitForRejectedWorkCommit(t, w)

	var state sandrunv1.RunnerState
	var readyTime *time.Time
	readRunner := func() {
		t.Helper()
		if err := w.env.DB.Pool.QueryRow(t.Context(), "SELECT state, ready_time FROM runners WHERE runner_id = $1", runnerID).Scan(&state, &readyTime); err != nil {
			t.Fatal(err)
		}
	}
	readRunner()
	if state != preparing || readyTime != nil {
		t.Fatalf("runner after rollback = %s, ready_time %v, want PREPARING and no timestamp", state, readyTime)
	}
	assertWorkMetric(t, reader, "sandrun_runner_start_seconds", workLabels("guest_os", "LINUX"), 0, 0)
	if got := transitionLogs(t, w, runnerID, "runner_state_changed", "PREPARING", "WARM", "NONE"); got != 0 {
		t.Fatalf("runner readiness logs after rollback = %d", got)
	}

	failCommit()
	eventually(t, "runner readiness commit and log", func() bool {
		readRunner()
		return state == warm && readyTime != nil &&
			transitionLogs(t, w, runnerID, "runner_state_changed", "PREPARING", "WARM", "NONE") == 1 &&
			workMetricCount(t, reader, "sandrun_runner_start_seconds", workLabels("guest_os", "LINUX")) == 1
	})
	assertWorkMetric(t, reader, "sandrun_runner_start_seconds", workLabels("guest_os", "LINUX"), 1, 13)

	// Leave no capacity for a replacement, so the metric stays specific to this runner.
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) { o.MaxSandboxes = 1 })
	session := w.createSession(t, w.env.User(w.org.Owner), profileID)
	w.waitState(t, session.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_RUNNING)
	eventually(t, "busy transition of the same runner", func() bool {
		return transitionLogs(t, w, runnerID, "runner_state_changed", "WARM", "BUSY", "NONE") == 1 &&
			transitionLogs(t, w, session.GetDevSessionId(), "request_state_changed", "STARTING", "RUNNING", "NONE") == 1
	})
	assertWorkMetric(t, reader, "sandrun_runner_start_seconds", workLabels("guest_os", "LINUX"), 1, 13)
	t.Log("A rejected readiness commit left the runner PREPARING with no ready_time, WARM log or startup sample. The retry produced one WARM log and a 13-second startup sample. BUSY added no startup sample.")
}

func newWorkTelemetryWorld(t *testing.T) (*workWorld, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	w := &workWorld{
		world: newWorld(t, func(o *app.Options) { o.Metrics = provider }),
		osOrg: api.NewID(),
	}
	w.fake.AddOrg(w.osOrg, ownsandKey('a'))
	w.fake.Update(w.osOrg, func(o *ownsandfake.Org) {
		o.CreateError = connect.NewWireError(connect.CodeUnavailable, errors.New("no device has free capacity"))
		o.StartRunning = false
	})
	if _, err := w.setLink(w.org.Admin, w.osOrg, ownsandKey('a')); err != nil {
		t.Fatal(err)
	}
	return w, reader
}

func execTelemetrySQL(t *testing.T, w *workWorld, sql string) {
	t.Helper()
	if _, err := w.env.DB.Pool.Exec(t.Context(), sql); err != nil {
		t.Fatal(err)
	}
}

func rejectWorkTransitionCommit(t *testing.T, w *workWorld, table string, from, to int16) func() {
	t.Helper()
	execTelemetrySQL(t, w, fmt.Sprintf(`
		CREATE FUNCTION reject_work_commit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'injected work transition commit failure' USING ERRCODE = '23514';
		END;
		$$;
		CREATE CONSTRAINT TRIGGER reject_work_commit AFTER UPDATE ON %s
			DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (OLD.state = %d AND NEW.state = %d)
			EXECUTE FUNCTION reject_work_commit();`, table, from, to))
	return func() {
		t.Helper()
		execTelemetrySQL(t, w, "DROP TRIGGER reject_work_commit ON "+table)
	}
}

func waitForRejectedWorkCommit(t *testing.T, w *workWorld) {
	t.Helper()
	eventually(t, "logged injected commit error", func() bool {
		return strings.Contains(w.env.Logs.String(), "injected work transition commit failure")
	})
}

func transitionLogs(t *testing.T, w *workWorld, id, msg, from, to, reason string) int {
	t.Helper()
	idKey := "request_id"
	if msg == "runner_state_changed" {
		idKey = "runner_id"
	}
	var count int
	for line := range strings.SplitSeq(w.env.Logs.String(), "\n") {
		if !strings.Contains(line, msg) || !strings.Contains(line, id) {
			continue
		}
		var log map[string]any
		if err := json.Unmarshal([]byte(line), &log); err != nil {
			t.Fatal(err)
		}
		if log["msg"] == msg && log[idKey] == id && log["previous_state"] == from && log["state"] == to && log["end_reason"] == reason {
			count++
		}
	}
	return count
}

func workLabels(pairs ...string) attribute.Set {
	attrs := []attribute.KeyValue{attribute.String("kind", "DEV_SESSION")}
	for i := 0; i < len(pairs); i += 2 {
		attrs = append(attrs, attribute.String(pairs[i], pairs[i+1]))
	}
	return attribute.NewSet(attrs...)
}

func workMetricCount(t *testing.T, reader *sdkmetric.ManualReader, name string, labels attribute.Set) uint64 {
	t.Helper()
	count, _ := workMetric(t, reader, name, labels)
	return count
}

func assertWorkMetric(t *testing.T, reader *sdkmetric.ManualReader, name string, labels attribute.Set, wantCount uint64, wantSum float64) {
	t.Helper()
	count, sum := workMetric(t, reader, name, labels)
	if count != wantCount || sum != wantSum {
		t.Fatalf("%s count = %d, sum = %g, want %d and %g", name, count, sum, wantCount, wantSum)
	}
}

func workMetric(t *testing.T, reader *sdkmetric.ManualReader, name string, labels attribute.Set) (uint64, float64) {
	t.Helper()
	var collected metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &collected); err != nil {
		t.Fatal(err)
	}
	var count uint64
	var sum float64
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range data.DataPoints {
					if !p.Attributes.Equals(&labels) {
						t.Fatalf("%s unexpected labels %v", name, p.Attributes.ToSlice())
					}
					count += uint64(p.Value)
				}
			case metricdata.Histogram[float64]:
				for _, p := range data.DataPoints {
					if !p.Attributes.Equals(&labels) {
						t.Fatalf("%s unexpected labels %v", name, p.Attributes.ToSlice())
					}
					count += p.Count
					sum += p.Sum
				}
			default:
				t.Fatalf("%s unexpected aggregation %T", name, data)
			}
		}
	}
	return count, sum
}
