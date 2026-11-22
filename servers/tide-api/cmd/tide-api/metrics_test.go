// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/delinoio/mono/servers/internal/telemetry"
)

// TestMetricsServer checks that the internal listener serves only
// GET /metrics from the OpenTelemetry meter provider.
func TestMetricsServer(t *testing.T) {
	ctx := context.Background()
	tel, err := telemetry.New(ctx, telemetry.Options{
		ServiceName: "tide-api",
		Stdout:      io.Discard,
		LookupEnv:   func(string) (string, bool) { return "", false },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer tel.Shutdown(ctx)
	g, err := tel.MeterProvider.Meter("test").Int64Gauge("tide_test_gauge")
	if err != nil {
		t.Fatal(err)
	}
	g.Record(ctx, 3)
	srv := httptest.NewServer(newMetricsServer(tel, slog.New(slog.NewJSONHandler(io.Discard, nil))).Handler)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "tide_test_gauge 3") {
		t.Fatalf("metrics: %d %s", resp.StatusCode, body)
	}
	for _, path := range []string{"/", "/healthz", "/delino.tide.v1.OrganizationService/ListOrganizations"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
	}
}
