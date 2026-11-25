// SPDX-License-Identifier: FSL-1.1-ALv2

package metrics_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/delinoio/mono/servers/internal/telemetry"
	"github.com/delinoio/mono/servers/tide-api/internal/metrics"
)

func TestWorkGaugeLease(t *testing.T) {
	tel, err := telemetry.New(t.Context(), telemetry.Options{
		ServiceName: "tide-test", Stdout: io.Discard, LookupEnv: func(string) (string, bool) { return "", false },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := tel.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	m, err := metrics.New(tel.MeterProvider)
	if err != nil {
		t.Fatal(err)
	}

	scrape := func(want, absent []string) {
		t.Helper()
		recorder := httptest.NewRecorder()
		tel.MetricsHandler(nil).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("scrape HTTP status = %d", recorder.Code)
		}
		body := recorder.Body.String()
		for _, line := range want {
			if !strings.Contains(body, line) {
				t.Errorf("scrape lacks %q", line)
			}
		}
		for _, line := range absent {
			if strings.Contains(body, line) {
				t.Errorf("scrape includes %q", line)
			}
		}
	}

	const counter = `tide_webhook_deliveries_total{result="ignored",sender="github"} 1`
	const oldRequests = `tide_requests{kind="GITHUB_JOB",priority="HIGH",state="QUEUED"} 2`
	const oldRunners = `tide_runners{guest_os="LINUX",kind="GITHUB_JOB",state="BUSY"} 3`
	const newRequests = `tide_requests{kind="DEV_SESSION",priority="NORMAL",state="RUNNING"} 5`
	const newRunners = `tide_runners{guest_os="WINDOWS",kind="DEV_SESSION",state="WARM"} 6`
	gaugeNames := []string{"tide_requests", "tide_runners"}

	m.WebhookDelivery("github", "ignored")
	scrape([]string{counter}, gaugeNames)

	first, cancelFirst := context.WithCancel(t.Context())
	defer cancelFirst()
	firstRequests := map[[3]string]int64{{"GITHUB_JOB", "HIGH", "QUEUED"}: 2}
	firstRunners := map[[3]string]int64{{"GITHUB_JOB", "LINUX", "BUSY"}: 3}
	stopFirst := m.ObserveWorkGauges(first, func() (map[[3]string]int64, map[[3]string]int64) {
		return firstRequests, firstRunners
	})
	defer stopFirst()
	scrape([]string{counter, oldRequests, oldRunners}, nil)

	cancelFirst()
	// The canceled component can still be finishing its last sample.
	firstRequests = map[[3]string]int64{{"GITHUB_JOB", "HIGH", "QUEUED"}: 4}
	for range 3 {
		scrape([]string{counter}, gaugeNames)
	}

	second, cancelSecond := context.WithCancel(t.Context())
	defer cancelSecond()
	var secondRequests, secondRunners map[[3]string]int64
	stopSecond := m.ObserveWorkGauges(second, func() (map[[3]string]int64, map[[3]string]int64) {
		return secondRequests, secondRunners
	})
	defer stopSecond()
	scrape([]string{counter}, gaugeNames)

	secondRequests = map[[3]string]int64{{"DEV_SESSION", "NORMAL", "RUNNING"}: 5}
	secondRunners = map[[3]string]int64{{"DEV_SESSION", "WINDOWS", "WARM"}: 6}
	stopFirst()
	scrape([]string{counter, newRequests, newRunners}, []string{`kind="GITHUB_JOB"`})

	stopSecond()
	scrape([]string{counter}, gaugeNames)
}
