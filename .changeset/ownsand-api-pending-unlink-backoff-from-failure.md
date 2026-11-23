---
"ownsand-api": patch
---

Start the retry back-off of a failed pending unlink when the `UnlinkOrganization` call failed, so the wait for the organization lock and the call itself no longer consume the delay.
