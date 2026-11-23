---
"ownsand-agent": patch
---

The agent now counts a sandbox in `DELETING` state as busy until its VM stop is confirmed. Before, a pending or failed stop could leave a live VM while a staged update installed and restarted the agent. A stop that the runtime confirmed, and the retry of a failed stop, behave as before.
