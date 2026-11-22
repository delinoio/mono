---
"ownsand-agent": patch
---

Record a sandbox as `PAUSED` when a macOS or Hyper-V snapshot paused it and could not resume it. The shared snapshot pause helper dropped the paused-source outcome, so the agent kept the record `RUNNING` and a later resume command returned success without reaching the paused VM. The helper now reports the paused source with the error, also when the capture failed first, so the agent publishes the paused state and one explicit resume reaches the runtime. Successful snapshots and sources that were already paused behave as before.
