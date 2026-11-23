---
"ownsand-cli": minor
---

Add `ownsand billing status`, which shows the linked billing account, the billing status and reason, the block and suspension times and, for an organization owner or admin, the balance and this month's usage and cost for each meter (text or `--format json`; members and API keys see the status only). A command that billing rejects prints `billing: <message>` with the `https://accounts.delino.io` link and exits 1, and `sandbox exec` exits 255. `ownsand device list` now shows the exact allocated vCPUs and the exact effective vCPU limit, including values above 2147483647, from `vcpu_total_count` and `effective_vcpu_limit_count`, and falls back to `vcpu_count` when an older server does not send the exact allocation.
