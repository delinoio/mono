// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// Report results of the reports_total metric.
const (
	resultCharged      = "charged"
	resultDuplicate    = "duplicate"
	resultUnattributed = "unattributed"
)

// meterName is the instrumentation scope of the billing metrics.
const meterName = "github.com/delinoio/mono/servers/ownsand-api/internal/billing"

// meter returns the billing meter of provider; a nil provider records
// nothing.
func meter(provider metric.MeterProvider) metric.Meter {
	if provider == nil {
		provider = noop.NewMeterProvider()
	}
	return provider.Meter(meterName)
}

// int64Counter and the other constructors report an invalid instrument
// through the OpenTelemetry error handler. The SDK then returns a working
// instrument, so the caller does not stop.
func int64Counter(m metric.Meter, name, help string) metric.Int64Counter {
	c, err := m.Int64Counter(name, metric.WithDescription(help))
	if err != nil {
		otel.Handle(err)
	}
	return c
}

func int64Gauge(m metric.Meter, name, help string) metric.Int64Gauge {
	g, err := m.Int64Gauge(name, metric.WithDescription(help))
	if err != nil {
		otel.Handle(err)
	}
	return g
}

func float64Gauge(m metric.Meter, name, unit, help string) metric.Float64Gauge {
	g, err := m.Float64Gauge(name, metric.WithUnit(unit), metric.WithDescription(help))
	if err != nil {
		otel.Handle(err)
	}
	return g
}

// add counts one event with attrs.
func add(c metric.Int64Counter, attrs ...attribute.KeyValue) {
	c.Add(context.Background(), 1, metric.WithAttributes(attrs...))
}

// metrics are the OpenTelemetry metrics of metering and reporting. Their
// labels never contain organization, user or billing account IDs. The names
// and labels are recorded in the Billing metrics section of
// docs/servers-ownsand-api-foundation.md. The instrument names have no
// _total or unit suffix: the Prometheus exporter adds them, so the served
// names stay the same as the names in the contract.
type metrics struct {
	outboxDepth    metric.Int64Gauge
	reportLag      metric.Float64Gauge
	reports        metric.Int64Counter
	reportFailures metric.Int64Counter
	sealedRows     metric.Int64Counter
	flushFailures  metric.Int64Counter
}

func newMetrics(provider metric.MeterProvider) *metrics {
	m := meter(provider)
	return &metrics{
		outboxDepth: int64Gauge(m, "ownsand_billing_usage_outbox_depth", "Usage outbox rows that accounts-api has not accepted yet."),
		reportLag: float64Gauge(m, "ownsand_billing_usage_report_lag", "s",
			"Seconds since the end of the oldest window with an unreported outbox row; 0 when every row is reported."),
		reports:        int64Counter(m, "ownsand_billing_usage_reports", "Outbox rows that accounts-api accepted, by result (charged, duplicate or unattributed)."),
		reportFailures: int64Counter(m, "ownsand_billing_usage_report_failures", "Failed ReportUsage requests, by Connect code."),
		sealedRows:     int64Counter(m, "ownsand_billing_usage_sealed_rows", "Outbox rows written by window seals, by meter."),
		flushFailures:  int64Counter(m, "ownsand_billing_usage_flush_failures", "Failed writes of in-memory usage counters to Postgres."),
	}
}
