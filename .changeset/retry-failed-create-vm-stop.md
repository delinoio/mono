---
"ownsand-agent": patch
---

Retry the stop of a VM that a failed create left behind while the agent runs, so a later successful stop releases the retained VM handle and files without an agent restart.
