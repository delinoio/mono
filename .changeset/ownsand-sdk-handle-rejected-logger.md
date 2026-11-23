---
"ownsand-sdk": patch
---

A logger that returns a rejected promise no longer raises an unhandled rejection in the host process. The SDK ignores the rejection and does not wait for the logger, so RPC results and log records stay the same.
