// SPDX-License-Identifier: FSL-1.1-ALv2

// Package metrics holds the Prometheus metrics of accounts-api and serves them
// on the internal listener `METRICS_ADDR`
// (docs/servers-accounts-api-foundation.md, Metrics). The listener is a
// separate HTTP server: the public handler never serves `/metrics`.
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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics are the accounts-api metrics in their own registry. A nil *Metrics
// records nothing, so tests may leave it out.
type Metrics struct {
	registry *prometheus.Registry

	webhooks      *prometheus.CounterVec
	topUpAttempts *prometheus.CounterVec
	topUpResults  *prometheus.CounterVec
	emails        *prometheus.CounterVec
	creditGrants  *prometheus.CounterVec
	autoTopUpOff  prometheus.Counter
	usageReports  *prometheus.CounterVec
	statusChanges *prometheus.CounterVec
	linkedOrgs    *prometheus.GaugeVec
}

// New returns the metrics with the Go runtime and process collectors.
func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		webhooks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "accounts_paddle_webhooks_total",
			Help: "Paddle webhook deliveries by event type and result.",
		}, []string{"event_type", "result"}),
		topUpAttempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "accounts_top_up_attempts_total",
			Help: "Paddle one-time charge calls of top-ups by kind and outcome.",
		}, []string{"kind", "outcome"}),
		topUpResults: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "accounts_top_up_results_total",
			Help: "Finished top-ups by kind and result.",
		}, []string{"kind", "result"}),
		emails: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "accounts_billing_emails_total",
			Help: "Billing notification emails by kind and result.",
		}, []string{"kind", "result"}),
		creditGrants: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "accounts_credit_grants_total",
			Help: "Credit grants created from verified Paddle webhooks by source.",
		}, []string{"source"}),
		autoTopUpOff: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "accounts_auto_top_up_disabled_total",
			Help: "Automatic top-up settings turned off after consecutive failures.",
		}),
		usageReports: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "accounts_usage_reports_total",
			Help: "Usage reports by service and result.",
		}, []string{"service", "result"}),
		statusChanges: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "accounts_billing_status_changes_total",
			Help: "Billing status changes of linked organizations by the new status.",
		}, []string{"status"}),
		linkedOrgs: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "accounts_linked_organizations",
			Help: "Organizations that are linked to a billing account now, by billing status.",
		}, []string{"status"}),
	}
	m.registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.webhooks, m.topUpAttempts, m.topUpResults, m.emails, m.creditGrants, m.autoTopUpOff,
		m.usageReports, m.statusChanges, m.linkedOrgs,
	)
	return m
}

// Webhook counts one webhook delivery.
func (m *Metrics) Webhook(eventType, result string) {
	if m != nil {
		m.webhooks.WithLabelValues(eventType, result).Inc()
	}
}

// TopUpAttempt counts one Paddle charge call.
func (m *Metrics) TopUpAttempt(kind, outcome string) {
	if m != nil {
		m.topUpAttempts.WithLabelValues(kind, outcome).Inc()
	}
}

// TopUpResult counts one finished top-up.
func (m *Metrics) TopUpResult(kind, result string) {
	if m != nil {
		m.topUpResults.WithLabelValues(kind, result).Inc()
	}
}

// Email counts one notification email.
func (m *Metrics) Email(kind, result string) {
	if m != nil {
		m.emails.WithLabelValues(kind, result).Inc()
	}
}

// CreditGrant counts one credit grant.
func (m *Metrics) CreditGrant(source string) {
	if m != nil {
		m.creditGrants.WithLabelValues(source).Inc()
	}
}

// AutoTopUpDisabled counts one automatic shutoff.
func (m *Metrics) AutoTopUpDisabled() {
	if m != nil {
		m.autoTopUpOff.Inc()
	}
}

// UsageReport counts one usage report.
func (m *Metrics) UsageReport(service, result string) {
	if m != nil {
		m.usageReports.WithLabelValues(service, result).Inc()
	}
}

// StatusChange counts one billing status change of a linked organization.
func (m *Metrics) StatusChange(status string) {
	if m != nil {
		m.statusChanges.WithLabelValues(status).Inc()
	}
}

// LinkedOrganizations sets the number of linked organizations with status.
func (m *Metrics) LinkedOrganizations(status string, n int64) {
	if m != nil {
		m.linkedOrgs.WithLabelValues(status).Set(float64(n))
	}
}

// Handler serves the metrics in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{}))
	return mux
}

// shutdownTimeout bounds the drain of the metrics listener.
const shutdownTimeout = 5 * time.Second

// Serve serves the metrics on listener until ctx is done. It returns nil after
// a shutdown and the listener error otherwise.
func Serve(ctx context.Context, listener net.Listener, m *Metrics, logger *slog.Logger) error {
	srv := &http.Server{
		Handler:           m.Handler(),
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
