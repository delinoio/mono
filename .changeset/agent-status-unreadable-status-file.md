---
"ownsand-agent": patch
---

`ownsand-agent status` now reports a `status.json` that cannot be read or decoded as an error: it prints a diagnostic to standard error and exits with `1`, where before it printed `Connection: unknown (the agent has not run)` and exited with `0`. An absent `status.json` keeps the first-run message and exit code `0`.
