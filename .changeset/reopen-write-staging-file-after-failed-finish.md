---
"ownsand-agent": patch
---

The guest helper now reopens the retained staging file of a write at the start of every later chunk or finish when a failed finish could not reopen it, instead of failing every retry with `write: staging file is not open` until the write expires. When the staging file is gone or was replaced by another file, the write ends with `not_found` (`write staging file is gone`) and the next sweep removes its record.
