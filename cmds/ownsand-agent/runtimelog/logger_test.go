// SPDX-License-Identifier: FSL-1.1-ALv2

package runtimelog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

type recorder struct {
	records []sdklog.Record
}

func (r *recorder) Export(_ context.Context, records []sdklog.Record) error {
	for _, record := range records {
		r.records = append(r.records, record.Clone())
	}
	return nil
}

func (*recorder) Shutdown(context.Context) error   { return nil }
func (*recorder) ForceFlush(context.Context) error { return nil }

func TestReviewedRuntimeFields(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")
	for _, tc := range []struct {
		component Component
		message   string
		field     string
		value     string
	}{
		{Agent, "authentication failed", "runtime", "firecracker"},
		{Guest, "log rotation failed; appending to the active log", "operation", "rotate"},
	} {
		t.Run(string(tc.component), func(t *testing.T) {
			var local bytes.Buffer
			exporter := &recorder{}
			logger, shutdown := New(&local, slog.LevelWarn, tc.component, sdklog.NewSimpleProcessor(exporter))
			t.Cleanup(shutdown)
			logger = logger.With("component", "private-fake-component")
			logger.Info(tc.message, tc.field, tc.value)
			logger.Warn("private-dynamic-message")
			logger.WithGroup("details").With(tc.field, tc.value).Warn(tc.message,
				tc.field, errors.New("private-error-in-approved-field"),
				"error", errors.New("private-error"),
				"token", "private-token",
				"url", "private-signed-url",
				"sdp", "private-sdp",
				"command", "private-command",
				"env", "private-env",
				"output", "private-output",
				"content", "private-content",
				slog.Group("unknown_group", tc.field, "private-nested-value"))
			raw := strings.Split(strings.TrimSpace(local.String()), "\n")
			if len(raw) != 2 {
				t.Fatalf("local record count = %d, want 2: %s", len(raw), &local)
			}
			var localRecord struct {
				Level   string         `json:"level"`
				Message string         `json:"msg"`
				Details map[string]any `json:"details"`
			}
			if err := json.Unmarshal([]byte(raw[1]), &localRecord); err != nil {
				t.Fatal(err)
			}
			if localRecord.Level != "WARN" || localRecord.Message != tc.message || localRecord.Details["output"] != "private-output" {
				t.Fatalf("unexpected local record: %s", raw[1])
			}
			if len(exporter.records) != 1 {
				t.Fatalf("SDK records = %d, want 1", len(exporter.records))
			}
			record := exporter.records[0]
			if record.Body().AsString() != tc.message || record.Severity() != 13 || record.SeverityText() != "WARN" {
				t.Fatalf("SDK body and severity = %v, %v, %s", record.Body(), record.Severity(), record.SeverityText())
			}
			fields := make(map[string]attribute.Value)
			record.WalkAttributes(func(kv attribute.KeyValue) bool {
				fields[string(kv.Key)] = kv.Value
				return true
			})
			if len(fields) != 2 || fields["component"].AsString() != string(tc.component) {
				t.Fatalf("SDK fields = %v, want component and details", fields)
			}
			entries := fields["details"].AsMap()
			if len(entries) != 1 || string(entries[0].Key) != tc.field || entries[0].Value.AsString() != tc.value {
				t.Fatalf("SDK details = %v, want %s=%s", entries, tc.field, tc.value)
			}

			// Write the same group as an inline attribute. It must obey the
			// same policy as the logger's WithGroup and With methods.
			logger.Warn(tc.message, slog.Group("details", tc.field, tc.value, "error", errors.New("private-nested-error")),
				tc.field, "private-unreviewed-category")
			if len(exporter.records) != 2 {
				t.Fatalf("SDK records = %d, want 2", len(exporter.records))
			}
			got := make(map[string]attribute.Value)
			exporter.records[1].WalkAttributes(func(kv attribute.KeyValue) bool {
				got[string(kv.Key)] = kv.Value
				return true
			})
			if len(got) != 2 || !reflect.DeepEqual(got["details"], fields["details"]) {
				t.Fatalf("SDK fields differ for an inline group: %v", got)
			}
		})
	}
}

func TestExtraEnvironmentResourceStaysOutOfProcessor(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "private.key=private-value")
	var local bytes.Buffer
	exporter := &recorder{}
	logger, shutdown := New(&local, slog.LevelInfo, Guest, sdklog.NewSimpleProcessor(exporter))
	defer shutdown()
	logger.Error("listen failed")
	if !strings.Contains(local.String(), "listen failed") || len(exporter.records) != 0 {
		t.Fatalf("local JSON = %q, SDK records = %d", &local, len(exporter.records))
	}
}

type blockingExporter struct {
	recorder
	started chan struct{}
}

func (e *blockingExporter) Export(ctx context.Context, _ []sdklog.Record) error {
	close(e.started)
	<-ctx.Done()
	return ctx.Err()
}

func TestBatchShutdownDeadlinePreservesLocalJSON(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")
	var local bytes.Buffer
	exporter := &blockingExporter{started: make(chan struct{})}
	processor := sdklog.NewBatchProcessor(exporter, sdklog.WithExportInterval(time.Hour))
	logger, shutdown := New(&local, slog.LevelInfo, Guest, processor)
	logger.Error("listen failed")
	if !strings.Contains(local.String(), `"level":"ERROR","msg":"listen failed","component":"ownsand-guest"`) {
		t.Fatalf("local JSON = %q", &local)
	}
	start := time.Now()
	shutdown()
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("shutdown took %s, want approximately one second", elapsed)
	}
	select {
	case <-exporter.started:
	default:
		t.Fatal("batch processor did not attempt export")
	}
}
