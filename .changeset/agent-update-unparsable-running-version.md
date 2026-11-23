---
"ownsand-agent": patch
---

An agent whose running version is not a SemVer string (for example a custom build name) now processes a valid pinned update target. Before, the failed comparison counted as equal versions, so the agent reported `UP_TO_DATE` and never verified the manifest or installed the target. The agent now reaches manifest and artifact verification, then waits for idle (`PENDING_IDLE`) and installs the target once with one restart request. The agent reports `UP_TO_DATE` only when both versions parse and are equal, and writes the "installs a lower version" log line only when the comparison succeeds. An invalid target, an untrusted manifest signature and the server's default-version selection behave as before.
