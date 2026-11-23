---
"accounts-api": patch
"ownsand-api": patch
---

Flush the OTLP log export in parallel with the metrics export at shutdown. A metrics collector that does not answer no longer uses up the shutdown deadline before the queued log records are sent.
