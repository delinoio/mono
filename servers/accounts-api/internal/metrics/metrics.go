// SPDX-License-Identifier: FSL-1.1-ALv2

// Package metrics holds the OpenTelemetry metrics of accounts-api. The
// internal listener `METRICS_ADDR` serves them in the Prometheus format
// (docs/servers-accounts-api-foundation.md, Metrics). The listener is a
// separate HTTP server: the public handler never serves `/metrics`.
//
// The instrument names have no _total suffix: the Prometheus exporter adds
// it to counters, so the served names stay the same as the names in the
// contract.
//
// Labels hold only bounded enum values. They never contain billing account
// IDs, organization IDs, user IDs, email addresses or Paddle IDs.
package metrics

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// meterName is the instrumentation scope of the accounts-api metrics.
const meterName = "github.com/delinoio/mono/servers/accounts-api/internal/metrics"

// Metrics are the accounts-api metrics. A nil *Metrics records nothing, so
// tests may leave it out.
type Metrics struct {
	webhooks      metric.Int64Counter
	topUpAttempts metric.Int64Counter
	topUpResults  metric.Int64Counter
	emails        metric.Int64Counter
	creditGrants  metric.Int64Counter
	autoTopUpOff  metric.Int64Counter
	usageReports  metric.Int64Counter
	statusChanges metric.Int64Counter
	linkedOrgs    metric.Int64Gauge
}

// New creates the metrics with the meters of provider.
func New(provider metric.MeterProvider) (*Metrics, error) {
	meter := provider.Meter(meterName)
	var m Metrics
	var errs []error
	counter := func(name, help string) metric.Int64Counter {
		c, err := meter.Int64Counter(name, metric.WithDescription(help))
		errs = append(errs, err)
		return c
	}
	m.webhooks = counter("accounts_paddle_webhooks", "Paddle webhook deliveries by event type and result.")
	m.topUpAttempts = counter("accounts_top_up_attempts", "Paddle one-time charge calls of top-ups by kind and outcome.")
	m.topUpResults = counter("accounts_top_up_results", "Finished top-ups by kind and result.")
	m.emails = counter("accounts_billing_emails", "Billing notification emails by kind and result.")
	m.creditGrants = counter("accounts_credit_grants", "Credit grants created from verified Paddle webhooks by source.")
	m.autoTopUpOff = counter("accounts_auto_top_up_disabled", "Automatic top-up settings turned off after consecutive failures.")
	m.usageReports = counter("accounts_usage_reports", "Usage reports by service and result.")
	m.statusChanges = counter("accounts_billing_status_changes", "Billing status changes of linked organizations by the new status.")
	gauge, err := meter.Int64Gauge("accounts_linked_organizations", metric.WithDescription("Organizations that are linked to a billing account now, by billing status."))
	errs = append(errs, err)
	m.linkedOrgs = gauge
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &m, nil
}

func add(c metric.Int64Counter, attrs ...attribute.KeyValue) {
	c.Add(context.Background(), 1, metric.WithAttributes(attrs...))
}

// Webhook counts one webhook delivery.
func (m *Metrics) Webhook(eventType, result string) {
	if m != nil {
		add(m.webhooks, attribute.String("event_type", eventType), attribute.String("result", result))
	}
}

// TopUpAttempt counts one Paddle charge call.
func (m *Metrics) TopUpAttempt(kind, outcome string) {
	if m != nil {
		add(m.topUpAttempts, attribute.String("kind", kind), attribute.String("outcome", outcome))
	}
}

// TopUpResult counts one finished top-up.
func (m *Metrics) TopUpResult(kind, result string) {
	if m != nil {
		add(m.topUpResults, attribute.String("kind", kind), attribute.String("result", result))
	}
}

// Email counts one notification email.
func (m *Metrics) Email(kind, result string) {
	if m != nil {
		add(m.emails, attribute.String("kind", kind), attribute.String("result", result))
	}
}

// CreditGrant counts one credit grant.
func (m *Metrics) CreditGrant(source string) {
	if m != nil {
		add(m.creditGrants, attribute.String("source", source))
	}
}

// AutoTopUpDisabled counts one automatic shutoff.
func (m *Metrics) AutoTopUpDisabled() {
	if m != nil {
		add(m.autoTopUpOff)
	}
}

// UsageReport counts one usage report.
func (m *Metrics) UsageReport(service, result string) {
	if m != nil {
		add(m.usageReports, attribute.String("service", service), attribute.String("result", result))
	}
}

// StatusChange counts one billing status change of a linked organization.
func (m *Metrics) StatusChange(status string) {
	if m != nil {
		add(m.statusChanges, attribute.String("status", status))
	}
}

// LinkedOrganizations sets the number of linked organizations with status.
func (m *Metrics) LinkedOrganizations(status string, n int64) {
	if m != nil {
		m.linkedOrgs.Record(context.Background(), n, metric.WithAttributes(attribute.String("status", status)))
	}
}

// shutdownTimeout bounds the drain of the metrics listener.
const shutdownTimeout = 5 * time.Second

// Serve serves handler, the Prometheus exposition of the metrics, on listener
// until ctx is done. It returns nil after a shutdown and the listener error
// otherwise.
func Serve(ctx context.Context, listener net.Listener, handler http.Handler, logger *slog.Logger) error {
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(listener) }()
	logger.Info("metrics_listener_started", slog.String("addr", listener.Addr().String()))
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	err := srv.Shutdown(shutdownCtx)
	if serveErr := <-errc; !errors.Is(serveErr, http.ErrServerClosed) {
		err = errors.Join(err, serveErr)
	}
	return err
}
