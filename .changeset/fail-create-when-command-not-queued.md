---
"ownsand-api": patch
---

Fail a `CreateSandbox` whose create command is not queued while the device channel stays connected. When the request context ends while a full outbound queue holds the send, the server attempts the move to `FAILED` with `CREATE_FAILED` within a bounded database context that does not depend on the request. When the guarded update succeeds, the sandbox is `FAILED` and its reserved capacity is released, instead of the sandbox staying `CREATING` until the device reconnects, and the RPC returns the reloaded failed record. When only the reload fails, the RPC returns the `CREATING` record that it already held while the persisted state is `FAILED`, and a later replay of the same idempotency key returns the persisted state. When the transition cannot be applied, the sandbox stays `CREATING` with its reservation, the server closes the current channel, and reconnect reconciliation ends the sandbox.
