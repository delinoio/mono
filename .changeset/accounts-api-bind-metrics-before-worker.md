---
"accounts-api": patch
---

Bind the metrics listener before the billing worker starts. A `METRICS_ADDR` that cannot be bound now stops the server with an error before any billing work begins, instead of returning while the worker, and a Paddle call it may start, still runs.
