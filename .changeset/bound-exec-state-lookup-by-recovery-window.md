---
"ownsand-sdk": patch
---

The pre-start recovery of an `exec` now bounds its sandbox state lookup by the relay recovery window. Before, a `GetSandbox` call that stalled after `AttachExec` answered `not_found` could keep the operation pending beyond the 60-second deadline. The operation now ends at the deadline with the last relay failure and sends no further `ExecRequest`, and a caller abort still returns the caller's reason.
