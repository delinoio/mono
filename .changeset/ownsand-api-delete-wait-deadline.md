---
"ownsand-api": patch
---

Apply one wait deadline to DeleteSandbox state reads and return the last observed state if the deadline expires between reads. A state read that uses up the remaining time returns deadline_exceeded and preserves the committed delete intent.
