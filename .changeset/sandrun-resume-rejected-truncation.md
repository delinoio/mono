---
"sandrun-cli": patch
---

Preserve both resume offsets when the SandRun Go SDK callback rejects a truncation notice, so an attach can report the missing output again.
