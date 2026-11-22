// SPDX-License-Identifier: FSL-1.1-ALv2

package billing_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// testMeter is an OpenTelemetry meter provider whose metrics the tests read
// directly.
type testMeter struct {
	*sdkmetric.MeterProvider
	reader *sdkmetric.ManualReader
}

func newTestMeter(t *testing.T) *testMeter {
	reader := sdkmetric.NewManualReader()
	m := &testMeter{MeterProvider: sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)), reader: reader}
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	return m
}

// gauge returns the last value of the gauge name with attrs, and false when
// the gauge has no such value.
func (m *testMeter) gauge(t *testing.T, name string, attrs ...attribute.KeyValue) (float64, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := m.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	want := attribute.NewSet(attrs...)
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != name {
				continue
			}
			switch data := md.Data.(type) {
			case metricdata.Gauge[int64]:
				for _, dp := range data.DataPoints {
					if dp.Attributes.Equals(&want) {
						return float64(dp.Value), true
					}
				}
			case metricdata.Gauge[float64]:
				for _, dp := range data.DataPoints {
					if dp.Attributes.Equals(&want) {
						return dp.Value, true
					}
				}
			default:
				t.Fatalf("%s is %T, not a gauge", name, md.Data)
			}
		}
	}
	return 0, false
}
