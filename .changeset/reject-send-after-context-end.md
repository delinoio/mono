---
"ownsand-api": patch
---

Fail an agent channel send whose request context already ended instead of queuing the envelope when the outbound queue has room. A canceled request no longer sends a command that its handler then treats as failed, so `CreateSnapshot` and `ExportSnapshot` end the attempt as `FAILED` or `EXPORT_FAILED` without a command reaching the agent.
