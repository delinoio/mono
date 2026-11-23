---
"ownsand-agent": patch
---

On Windows, a concurrent `ownsand-agent status` or another reader of a state file no longer makes the running agent lose a `status.json` update. The atomic replacement of a state file now retries for about one second while another process has the file open, before it reports the error.
