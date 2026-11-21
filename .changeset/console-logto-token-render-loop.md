---
"ownsand-console": patch
---

Stop the endless re-rendering of the console after sign-in with Logto: a page that requested an access token no longer unmounts itself and mounts again in a loop.
