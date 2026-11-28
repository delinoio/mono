---
"sandrun-cli": patch
---

Discard exec stream events that arrive after the caller context ends. Preserve the last acknowledged output offsets so the caller can attach again.
