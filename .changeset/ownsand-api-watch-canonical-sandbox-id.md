---
"ownsand-api": patch
---

Wake the sandbox watchers of waiting `DeleteSandbox` calls at once when an agent sandbox update or command result spells the sandbox ID in uppercase, instead of at the next one second poll.
