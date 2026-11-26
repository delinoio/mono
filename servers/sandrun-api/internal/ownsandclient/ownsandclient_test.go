// SPDX-License-Identifier: FSL-1.1-ALv2

package ownsandclient_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/sandrun-api/internal/metrics"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient/ownsandfake"
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
	c := &ownsandclient.Instrumented{Inner: inner, Metrics: m, Logger: slog.New(slog.NewJSONHandler(&logs, nil)), OrganizationID: "sandrun-org", OnUnauthenticated: func() { unauth++ }}
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
			if metric.Name != "sandrun_ownsand_calls" {
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

func TestSDKDeleteSandboxDeadline(t *testing.T) {
	requests := make(chan string, 1)
	canceled := make(chan struct{})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.URL.Path
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-t.Context().Done():
		}
	}))
	server.Config.Protocols = new(http.Protocols)
	server.Config.Protocols.SetUnencryptedHTTP2(true)
	server.Start()
	t.Cleanup(server.Close)

	factory := ownsandclient.SDKFactory{BaseURL: server.URL}
	client, err := factory.New("org-1", "osk_"+strings.Repeat("a", 43))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()

	if _, err := client.DeleteSandbox(ctx, "sandbox-1"); connect.CodeOf(err) != connect.CodeDeadlineExceeded {
		t.Fatalf("DeleteSandbox error = %v, want deadline_exceeded", err)
	}
	select {
	case path := <-requests:
		if path != ownsandv1connect.SandboxServiceDeleteSandboxProcedure {
			t.Fatalf("request path = %q, want DeleteSandbox", path)
		}
	default:
		t.Fatal("the server never received the DeleteSandbox attempt")
	}
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("the server did not observe request cancellation")
	}
}
