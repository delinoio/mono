---
"accounts-api": patch
---

Reject a Paddle success answer whose `data` member is `null`. Before, the client decoded it into a zero entity and returned it as a success, so a one-time charge counted as applied. The call now fails with an unknown outcome: a top-up moves to unknown, a retry with the same key sends no second charge, and a webhook that finalized the top-up keeps its result.
