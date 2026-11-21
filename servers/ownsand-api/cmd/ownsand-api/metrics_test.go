// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// TestMetricsServer checks that the internal listener serves only
// GET /metrics from its registry.
func TestMetricsServer(t *testing.T) {
	registry := prometheus.NewRegistry()
	g := prometheus.NewGauge(prometheus.GaugeOpts{Name: "ownsand_test_gauge", Help: "test"})
	g.Set(3)
	registry.MustRegister(g)
	srv := httptest.NewServer(newMetricsServer(registry, slog.New(slog.NewJSONHandler(io.Discard, nil))).Handler)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "ownsand_test_gauge 3") {
		t.Fatalf("metrics: %d %s", resp.StatusCode, body)
	}
	for _, path := range []string{"/", "/healthz", "/delino.ownsand.v1.OrganizationService/ListOrganizations"} {
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
