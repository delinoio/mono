---
"ownsand-api": patch
---

The Logto key set refresh no longer holds the validator lock during the fetch. A token with a cached key validates without waiting for a running or hung key set fetch. A token with an unknown key ID waits for the fetch or for its own request context, whichever ends first, and a canceled request leaves the fetch running.
