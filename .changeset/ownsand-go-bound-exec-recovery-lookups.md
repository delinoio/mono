---
"ownsand-sdk": patch
"ownsand-cli": patch
"sandrun-api": patch
---

The OwnSand Go SDK bounds sandbox state lookups during relay recovery for `Exec` and `AttachExec`. If the recovery window expires, the SDK cancels the lookup and returns the last relay failure in a data path error. An earlier caller cancellation takes precedence.
