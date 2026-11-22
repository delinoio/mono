---
"ownsand-api": patch
---

Fail a `CreateSandbox` whose create command is not queued while the device channel stays connected. When the request context ends while a full outbound queue holds the send, the sandbox now moves to `FAILED` with `CREATE_FAILED` and its reserved capacity is released, instead of staying `CREATING` until the device reconnects. The RPC and a replay of the same idempotency key return the failed record.
