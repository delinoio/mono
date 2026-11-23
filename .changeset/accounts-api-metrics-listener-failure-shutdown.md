---
"accounts-api": patch
---

Shut the server down when the metrics listener fails after it was bound. A terminal metrics listener error now logs `metrics_listener_failed`, drains the public server within the shutdown timeout, waits for the billing worker and exits non-zero with the original error, instead of serving on without metrics until a signal arrives.
