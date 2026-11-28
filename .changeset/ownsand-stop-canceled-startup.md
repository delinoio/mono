---
"ownsand-api": patch
---

Stop canceled startup before activating workers or serving traffic. Close any sockets already bound and finish database and telemetry cleanup.
