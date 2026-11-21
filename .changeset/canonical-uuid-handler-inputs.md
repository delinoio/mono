---
"ownsand-api": patch
---

Treat the uppercase and lowercase spellings of a UUID v7 request field as one identifier in every handler: R2 clean-up prefixes, sandbox placement, create idempotency, relay grants, agent commands, audit targets and responses now use the lowercase text. An `exec_id` and the `operation_id` of an `EXEC` or `ATTACH_EXEC` grant stay lowercase-only.
