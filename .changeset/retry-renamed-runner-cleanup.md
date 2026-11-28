---
"sandrun-api": patch
---

Keep the current GitHub runner reserve available when removing an old-label runner fails. Retry the same surplus check without waiting for another webhook.
