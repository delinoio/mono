---
"ownsand-api": patch
---

Keep the shared Logto key set refresh independent of the request that triggers it, so a canceled request no longer blocks valid requests during the refresh cooldown.
