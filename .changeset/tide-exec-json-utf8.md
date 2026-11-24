---
"tide-cli": patch
---

Preserve UTF-8 characters split across output chunks in `tide session exec` and `tide session attach` JSON output. Preserve pending bytes when resuming after an interrupt.
