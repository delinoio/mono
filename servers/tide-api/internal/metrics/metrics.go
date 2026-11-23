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
	ownsandCalls metric.Int64Counter
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
