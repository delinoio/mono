---
"sandrun-api": patch
---

Authorize organization deletion before runner and provider cleanup. Block competing deletions and ownership transfers during cleanup, and let the current owner retry an interrupted deletion.
