---
"ownsand-agent": patch
---

Keep a snapshot unavailable when a delete overtakes its creation. The agent can retry failed cleanup, and a restart cannot restore the abandoned capture as ready.
