---
"ownsand-sdk": patch
---

An `exec` whose resent `ExecRequest` fails with `failed_precondition` before it starts now reads the sandbox state. On `PAUSED` it waits for the sandbox to resume and resends the identical request with the original exec ID, and on a terminal state it returns the sandbox-ended result. Before, it ended with the `failed_precondition` error.
