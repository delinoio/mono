---
"ownsand-agent": patch
---

Windows services now use the explicit `--state-dir` for startup configuration errors. The diagnostic goes to `agent.log` in that directory, with the Windows Application Event Log as a fallback if the file cannot open.
