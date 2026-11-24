---
"ownsand-agent": patch
---

OwnSand keeps a revoked device out of sandbox placement while its agent retries an update. The agent reports the retry target and keeps rejecting new sandbox creates until a verified release manifest clears the revocation.
