---
"ownsand-api": patch
---

`CreateSandbox`, an idempotent replay, `GetSandbox` and `ListSandboxes` now return the exact `idle_ttl` and `max_ttl` of the request. Before, the server stored both TTLs in whole milliseconds, so a TTL below one millisecond or with a fraction of a millisecond, which the API accepts, was truncated and no longer matched the TTL in the create command that the agent received. Migration `0011_sandbox_ttl_nanoseconds.sql` adds the nullable columns `idle_ttl_ns` and `max_ttl_ns` to `sandboxes`; a row that an earlier release wrote keeps its whole-millisecond TTLs, and the millisecond columns are still written for a previous release that serves during a rolling deployment.
