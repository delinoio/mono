---
"sandrun-api": patch
---

Apply the sandbox deletion timeout when deleting a runner profile, removing an OwnSand link, or deleting an organization. A timed-out deletion keeps its runner, teardown target, and OwnSand credential available for retry.
