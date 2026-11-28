---
"sandrun-api": patch
---

Exit successfully when a termination signal cancels startup before SandRun begins serving. Close any listeners that already bound and skip lease worker startup when cancellation occurs before the workers start.
