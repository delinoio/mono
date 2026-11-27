---
"ownsand-agent": patch
---

`ownsand-agent status` now reports a read error and exits with code 1 when the status file contains JSON `null`. The command still prints both inventory counts.
