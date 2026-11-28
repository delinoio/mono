---
"ownsand-agent": patch
---

When creating a sandbox on Hyper-V, the OwnSand agent attempts to stop a leftover compute system for that sandbox before removing its VM directory. If the agent cannot confirm the stop, it logs a warning and still attempts removal. A leftover VM that still holds its disk open can cause a sharing violation.
