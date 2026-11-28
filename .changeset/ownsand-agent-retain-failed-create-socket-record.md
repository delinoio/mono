---
"ownsand-agent": patch
---

Keep the macOS process runtime's socket directory record when cleanup after a canceled restore or failed helper launch cannot remove the socket directory. The agent can then retry cleanup.
