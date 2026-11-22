---
"ownsand-api": patch
---

`ownsand-api` now keeps a pending command when a device other than the command's device reports its result. Before, the rejected result removed the record, so the right device could no longer complete the command and its result was dropped as unknown. A valid result still consumes the record exactly once.
