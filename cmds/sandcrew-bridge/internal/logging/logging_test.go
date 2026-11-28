// SPDX-License-Identifier: Apache-2.0

package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

type recorder struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func (r *recorder) Export(_ context.Context, records []sdklog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, record := range records {
		r.records = append(r.records, record.Clone())
	}
	return nil
}

func (*recorder) Shutdown(context.Context) error   { return nil }
func (*recorder) ForceFlush(context.Context) error { return nil }

func TestRecordsReachTheLocalJSONAndTheSDK(t *testing.T) {
	var local bytes.Buffer
	exporter := &recorder{}
	logger, shutdown := New(&local, slog.LevelInfo, sdklog.NewSimpleProcessor(exporter))
	logger = logger.With("bridge_version", "1.2.3")
	logger.Debug("hidden")
	logger.Info("agent started", "load_session", true)
	shutdown()

	var line map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(local.Bytes()), &line); err != nil {
		t.Fatalf("local output %q: %v", local.String(), err)
	}
	if line["msg"] != "agent started" || line["bridge_version"] != "1.2.3" || line["load_session"] != true {
		t.Fatalf("local record %v", line)
	}

	exporter.mu.Lock()
	defer exporter.mu.Unlock()
	if len(exporter.records) != 1 {
		t.Fatalf("SDK records %d, want 1 (the debug record is below the level)", len(exporter.records))
	}
	record := exporter.records[0]
	if record.Body().AsString() != "agent started" {
		t.Fatalf("SDK record body %q", record.Body().AsString())
	}
	attrs := map[string]string{}
	record.WalkAttributes(func(kv attribute.KeyValue) bool {
		attrs[string(kv.Key)] = kv.Value.Emit()
		return true
	})
	if attrs["bridge_version"] != "1.2.3" || attrs["load_session"] != "true" {
		t.Fatalf("SDK record attributes %v", attrs)
	}
	if got := record.InstrumentationScope().Name; got != ServiceName {
		t.Fatalf("scope %q, want %q", got, ServiceName)
	}
	found := false
	for _, kv := range record.Resource().Attributes() {
		found = found || (string(kv.Key) == "service.name" && kv.Value.AsString() == ServiceName)
	}
	if !found {
		t.Fatalf("resource %v has no service.name %s", record.Resource().Attributes(), ServiceName)
	}
}
