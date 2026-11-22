---
"ownsand-cli": patch
---

A command that refreshes the Logto access token now reloads the stored credentials first and uses a refresh that another `ownsand` process already finished, and it publishes its own result only over the refresh token it sent. Before, overlapping commands refreshed with a spent refresh token, failed with `invalid_grant`, or overwrote a newer rotated refresh token with an older one.
