---
"ownsand-agent": patch
---

The guest helper now resolves a bare program name of an `Exec` request with the `PATH` of the request `env` when the request sets one. Before, it searched only the `PATH` of the helper process, so a program that the request `PATH` names failed with `PROGRAM_NOT_FOUND`. A request without `PATH`, and a program name with a path separator, keep the earlier lookup, and the helper does not change its own environment. On a Windows guest the `PATH` key is matched without regard to case and the search uses `PATHEXT`.
