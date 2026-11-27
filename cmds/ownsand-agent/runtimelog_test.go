// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/config"
	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

type failingLogProcessor struct {
	records []sdklog.Record
	closed  bool
}

func (*failingLogProcessor) Enabled(context.Context, sdklog.EnabledParameters) bool { return true }
func (*failingLogProcessor) ForceFlush(context.Context) error                       { return nil }

func (p *failingLogProcessor) OnEmit(_ context.Context, record *sdklog.Record) error {
	p.records = append(p.records, record.Clone())
	return errors.New("fixture processor failed")
}

func (p *failingLogProcessor) Shutdown(context.Context) error {
	p.closed = true
	return errors.New("fixture shutdown failed")
}

func TestUnenrolledAgentBridgesStartupError(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")
	path := filepath.Join(t.TempDir(), "agent.log")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	prev := os.Stderr
	os.Stderr = file
	defer func() { os.Stderr = prev }()

	processor := &failingLogProcessor{}
	code, updated := runAgentWithInstaller(context.Background(), config.Config{StateDir: t.TempDir()}, nil, processor)
	if code != exitNotEnrolled || updated {
		t.Fatalf("run returned (%d, %t), want (%d, false)", code, updated, exitNotEnrolled)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var local struct {
		Level     string `json:"level"`
		Message   string `json:"msg"`
		Component string `json:"component"`
	}
	if err := json.Unmarshal(data, &local); err != nil {
		t.Fatal(err)
	}
	const message = "the device is not enrolled; run `ownsand-agent enroll`"
	if local.Level != "ERROR" || local.Message != message || local.Component != "ownsand-agent" {
		t.Fatalf("unexpected local record: %s", data)
	}
	if len(processor.records) != 1 || !processor.closed {
		t.Fatalf("SDK record count = %d, shutdown called = %t", len(processor.records), processor.closed)
	}
	record := processor.records[0]
	if record.Body().AsString() != message || record.SeverityText() != local.Level || record.Severity() != 17 {
		t.Fatalf("SDK body and severity = %v, %v, %s", record.Body(), record.Severity(), record.SeverityText())
	}
	fields := make(map[string]string)
	record.WalkAttributes(func(kv attribute.KeyValue) bool {
		fields[string(kv.Key)] = kv.Value.AsString()
		return true
	})
	if len(fields) != 1 || fields["component"] != local.Component {
		t.Fatalf("SDK fields = %v, want agent component only", fields)
	}
}
