---
"ownsand-cli": patch
---

A command whose Logto refresh was rejected with `invalid_grant` now waits up to 5 seconds for another `ownsand` process to publish the rotated refresh token before it reports a revoked token, and it ends that wait when the command is cancelled. Before, a command that overlapped with a refresh of another process failed at once with a revoked-token error while the other process was still storing the new token.
