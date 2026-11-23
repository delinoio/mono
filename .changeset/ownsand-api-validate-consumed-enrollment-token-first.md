---
"ownsand-api": patch
---

Reject a consumed enrollment token that is retried with another public key or without a recorded device as an invalid enrollment token before the billing check, so a caller that cannot use the token no longer sees the organization's billing error. An eligible same-key retry still returns the original device, or the billing error while billing blocks it.
