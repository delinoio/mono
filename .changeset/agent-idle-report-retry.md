---
"ownsand-agent": patch
---

The agent now keeps an idle-deadline report pending when it cannot persist the revision, so a later tick reports the moved deadline after storage recovers without new activity. An idle report that Tick collected for a sandbox that ended and was acknowledged in the meantime no longer recreates the dropped record file or publishes a new revision.
