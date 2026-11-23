---
"ownsand-api": patch
---

Keep the post-takeover cleanup guard of a pending billing unlink in an explicit column, so an owner-triggered unlink or replacement link cannot send the remote unlink or delete the recovery row while the old link call can still commit.
