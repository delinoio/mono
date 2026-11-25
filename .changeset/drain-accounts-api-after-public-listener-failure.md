---
"accounts-api": patch
---

Let active requests finish within the shutdown deadline after the public listener fails. Report the original listener error and any drain error at exit.
