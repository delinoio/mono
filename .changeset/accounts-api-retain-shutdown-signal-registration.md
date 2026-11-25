---
"accounts-api": patch
---

Preserve the Accounts API drain when the first termination signal arrives after a metrics listener failure. Finish in-flight work and report the original metrics error at exit.
