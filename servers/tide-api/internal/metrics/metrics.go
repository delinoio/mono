// SPDX-License-Identifier: FSL-1.1-ALv2

// Package metrics holds the OpenTelemetry instruments of tide-api. The
// owning contract is the Metrics section of
// docs/servers-tide-api-foundation.md. The instrument names have no `_total`
// suffix: the Prometheus exporter adds it to counters. Labels never contain
// organization IDs, user IDs, repository names or other user data.
package metrics

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

const meterName = "github.com/delinoio/mono/servers/tide-api"

// Metrics are the tide instruments. A nil *Metrics records nothing.
type Metrics struct {
	ownsandCalls  metric.Int64Counter
	requests      metric.Int64Gauge
	queueWait     metric.Float64Histogram
	requestsEnded metric.Int64Counter
	runners       metric.Int64Gauge
	runnerStart   metric.Float64Histogram
	devExecs      metric.Int64Counter
	transferBytes metric.Int64Counter
	// webhookDeliveries is tide_webhook_deliveries_total.
	webhookDeliveries metric.Int64Counter
}

// New creates the instruments with provider; nil selects a no-op provider.
func New(provider metric.MeterProvider) (*Metrics, error) {
	if provider == nil {
		provider = noop.NewMeterProvider()
	}
	meter := provider.Meter(meterName)
	var m Metrics
	var errs []error
	counter := func(name, help string) metric.Int64Counter {
		c, err := meter.Int64Counter(name, metric.WithDescription(help))
		errs = append(errs, err)
		return c
	}
	m.ownsandCalls = counter("tide_ownsand_calls", "Calls of the OwnSand API by RPC and Connect code.")
	m.requestsEnded = counter("tide_requests_ended", "Ended requests by kind, state and end reason.")
	m.devExecs = counter("tide_dev_session_execs", "Ended dev session execs by guest OS and result.")
	m.webhookDeliveries = counter("tide_webhook_deliveries", "Webhook deliveries by sender and result.")
	m.transferBytes = counter("tide_dev_session_transfer_bytes", "File bytes that tide-api relays for dev sessions, by direction and guest OS.")
	gauge := func(name, help string) metric.Int64Gauge {
		g, err := meter.Int64Gauge(name, metric.WithDescription(help))
		errs = append(errs, err)
		return g
	}
	m.requests = gauge("tide_requests", "Requests that are not ended, by kind, priority and state.")
	m.runners = gauge("tide_runners", "Runners that are not ended, by kind, guest OS and state.")
	histogram := func(name, help string) metric.Float64Histogram {
		h, err := meter.Float64Histogram(name, metric.WithDescription(help), metric.WithUnit("s"),
			metric.WithExplicitBucketBoundaries(1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600, 4*3600, 12*3600, 24*3600))
		errs = append(errs, err)
		return h
	}
	m.queueWait = histogram("tide_request_queue_wait_seconds", "Time from the queue time of a request until a runner takes it or it ends.")
	m.runnerStart = histogram("tide_runner_start_seconds", "Time from the creation of a runner until it is WARM, WAITING_FOR_WORK or BUSY.")
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &m, nil
}

// OwnSandCall counts one OwnSand API call.
func (m *Metrics) OwnSandCall(rpc, code string) {
	if m != nil {
		m.ownsandCalls.Add(context.Background(), 1, metric.WithAttributes(attribute.String("rpc", rpc), attribute.String("code", code)))
	}
}

// RequestEnded counts one ended request.
func (m *Metrics) RequestEnded(kind, state, endReason string) {
	if m != nil {
		m.requestsEnded.Add(context.Background(), 1, metric.WithAttributes(attribute.String("kind", kind), attribute.String("state", state), attribute.String("end_reason", endReason)))
	}
}

// QueueWait records the queue wait of a request that left the queue.
func (m *Metrics) QueueWait(kind, priority string, seconds float64) {
	if m != nil {
		m.queueWait.Record(context.Background(), seconds, metric.WithAttributes(attribute.String("kind", kind), attribute.String("priority", priority)))
	}
}

// RunnerStart records the start time of a runner.
func (m *Metrics) RunnerStart(kind, guestOS string, seconds float64) {
	if m != nil {
		m.runnerStart.Record(context.Background(), seconds, metric.WithAttributes(attribute.String("kind", kind), attribute.String("guest_os", guestOS)))
	}
}

// Requests records the number of open requests of one label set.
func (m *Metrics) Requests(kind, priority, state string, n int64) {
	if m != nil {
		m.requests.Record(context.Background(), n, metric.WithAttributes(attribute.String("kind", kind), attribute.String("priority", priority), attribute.String("state", state)))
	}
}

// Runners records the number of open runners of one label set.
func (m *Metrics) Runners(kind, guestOS, state string, n int64) {
	if m != nil {
		m.runners.Record(context.Background(), n, metric.WithAttributes(attribute.String("kind", kind), attribute.String("guest_os", guestOS), attribute.String("state", state)))
	}
}

// DevSessionExec counts one ended dev session exec; result is exited,
// failed or sandbox_ended.
func (m *Metrics) DevSessionExec(guestOS, result string) {
	if m != nil {
		m.devExecs.Add(context.Background(), 1, metric.WithAttributes(attribute.String("guest_os", guestOS), attribute.String("result", result)))
	}
}

// TransferBytes counts relayed dev session file bytes; direction is upload
// or download.
func (m *Metrics) TransferBytes(direction, guestOS string, n int64) {
	if m != nil && n > 0 {
		m.transferBytes.Add(context.Background(), n, metric.WithAttributes(attribute.String("direction", direction), attribute.String("guest_os", guestOS)))
	}
}

// WebhookDelivery counts one webhook delivery by sender (github, openai)
// and result (see webhook.Result).
func (m *Metrics) WebhookDelivery(sender, result string) {
	if m != nil {
		m.webhookDeliveries.Add(context.Background(), 1, metric.WithAttributes(attribute.String("sender", sender), attribute.String("result", result)))
	}
}
