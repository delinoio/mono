---
"ownsand-agent": patch
---

The Windows guest helper records a failed log rotation as the first new warning in the active log, even at the error log level. The configured level still filters routine logs.
