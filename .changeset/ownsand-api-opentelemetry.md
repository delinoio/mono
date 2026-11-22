---
"ownsand-api": minor
---

Record the billing metrics and logs with the OpenTelemetry SDK. `GET /metrics` on `METRICS_ADDR` keeps the same metric names and labels. The Go runtime metrics now use the OpenTelemetry names (for example `go_goroutine_count`), and the `process_*` metrics are removed. When `OTEL_EXPORTER_OTLP_ENDPOINT` is set, the server also exports the metrics and log records over OTLP/HTTP, for example to Grafana Cloud, with the headers from `OTEL_EXPORTER_OTLP_HEADERS`.
