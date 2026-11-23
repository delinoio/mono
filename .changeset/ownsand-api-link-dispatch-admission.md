---
"ownsand-api": patch
---

Start the LinkOrganization call of a billing link attempt only while the call can end before its persisted call deadline, so the cleanup worker cannot remove the attempt row while the call can still commit a remote link.
