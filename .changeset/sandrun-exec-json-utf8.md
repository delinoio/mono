---
"sandrun-cli": patch
---

Preserve UTF-8 characters split across output chunks in `sandrun session exec` and `sandrun session attach` JSON output. Preserve pending bytes when resuming after an interrupt.
