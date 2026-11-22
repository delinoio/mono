// SPDX-License-Identifier: FSL-1.1-ALv2

package ownsandclient_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"connectrpc.com/connect"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/delinoio/mono/servers/tide-api/internal/metrics"
	"github.com/delinoio/mono/servers/tide-api/internal/ownsandclient"
	"github.com/delinoio/mono/servers/tide-api/internal/ownsandclient/ownsandfake"
)

func TestInstrumentedCountsLogsAndReportsUnauthenticated(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	m, err := metrics.New(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	fake := ownsandfake.New()
	key := "osk_" + strings.Repeat("a", 43)
	fake.AddOrg("org-1", key)
	inner, _ := fake.New("org-1", key)
	unauth := 0
	c := &ownsandclient.Instrumented{Inner: inner, Metrics: m, Logger: slog.New(slog.NewJSONHandler(&logs, nil)), OrganizationID: "tide-org", OnUnauthenticated: func() { unauth++ }}
	ctx := context.Background()
	if _, err := c.ListDevices(ctx); err != nil {
		t.Fatal(err)
	}
	fake.RevokeKey("org-1")
	if _, err := c.ListDevices(ctx); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("revoked: %v", err)
	}
	if unauth != 1 {
		t.Fatalf("OnUnauthenticated ran %d times", unauth)
	}
	if !strings.Contains(logs.String(), `"rpc":"ListDevices","code":"ok"`) || !strings.Contains(logs.String(), `"code":"unauthenticated"`) ||
		strings.Contains(logs.String(), key) {
		t.Fatalf("logs: %s", logs.String())
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, metric := range sm.Metrics {
			if metric.Name != "tide_ownsand_calls" {
				continue
			}
			for _, dp := range metric.Data.(metricdata.Sum[int64]).DataPoints {
				rpc, _ := dp.Attributes.Value("rpc")
				code, _ := dp.Attributes.Value("code")
				got[rpc.AsString()+"/"+code.AsString()] = dp.Value
			}
		}
	}
	if got["ListDevices/ok"] != 1 || got["ListDevices/unauthenticated"] != 1 {
		t.Fatalf("metrics %v", got)
	}
	if ownsandclient.Code(nil) != "ok" || ownsandclient.Code(context.Canceled) != "canceled" {
		t.Fatal("code strings")
	}
}
