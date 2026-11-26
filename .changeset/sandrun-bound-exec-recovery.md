---
"sandrun-cli": patch
---

Bound exec reconnects when SandRun cannot deliver new command state or output. Preserve the exec ID and offsets so the command can be attached again.
