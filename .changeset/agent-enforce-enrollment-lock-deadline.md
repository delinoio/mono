---
"ownsand-agent": patch
---

Enforce the enrollment lock deadline for `ownsand-agent enroll` and `ownsand-agent reset`. Both commands report that the lock is busy if their wait ends before they can accept it. An expired enrollment leaves the identity untouched and sends no enrollment request. An expired reset preserves local state.
