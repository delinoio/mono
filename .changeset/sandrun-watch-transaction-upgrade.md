---
"sandrun-api": patch
---

Require a quiesced maintenance upgrade when SandRun first applies migration v9 to an initialized database. This prevents migration locks from deadlocking with older dispatch workers.
