// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Report results of the reports_total metric.
const (
	resultCharged      = "charged"
	resultDuplicate    = "duplicate"
	resultUnattributed = "unattributed"
)

// metrics are the Prometheus metrics of metering and reporting. Their labels
// never contain organization, user or billing account IDs. The names and
// labels are recorded in the Billing metrics section of
// docs/servers-ownsand-api-foundation.md.
type metrics struct {
	outboxDepth    prometheus.Gauge
	reportLag      prometheus.Gauge
	reports        *prometheus.CounterVec
	reportFailures *prometheus.CounterVec
	sealedRows     *prometheus.CounterVec
	flushFailures  prometheus.Counter
}

func newMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		outboxDepth: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ownsand_billing_usage_outbox_depth",
			Help: "Usage outbox rows that accounts-api has not accepted yet.",
		}),
		reportLag: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ownsand_billing_usage_report_lag_seconds",
			Help: "Seconds since the end of the oldest window with an unreported outbox row; 0 when every row is reported.",
		}),
		reports: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ownsand_billing_usage_reports_total",
			Help: "Outbox rows that accounts-api accepted, by result (charged, duplicate or unattributed).",
		}, []string{"result"}),
		reportFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ownsand_billing_usage_report_failures_total",
			Help: "Failed ReportUsage requests, by Connect code.",
		}, []string{"code"}),
		sealedRows: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ownsand_billing_usage_sealed_rows_total",
			Help: "Outbox rows written by window seals, by meter.",
		}, []string{"meter"}),
		flushFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ownsand_billing_usage_flush_failures_total",
			Help: "Failed writes of in-memory usage counters to Postgres.",
		}),
	}
	if reg != nil {
		reg.MustRegister(m.outboxDepth, m.reportLag, m.reports, m.reportFailures, m.sealedRows, m.flushFailures)
	}
	return m
}
