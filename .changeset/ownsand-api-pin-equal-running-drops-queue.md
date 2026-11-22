---
"ownsand-api": patch
---

When an organization pins the agent version that a device already runs, `ownsand-api` now sends the pin to the device once per channel and decision. Before, it sent nothing, so a device that held a queued newer update kept it and installed it when idle, against the pin. The agent drops the queued update on the command and reports `UP_TO_DATE`.
