---
"accounts-api": patch
---

The billing worker checks its 30-second budget before each automatic top-up claim. A top-up that remains unclaimed stays pending for a later run. One committed charge can continue after the budget expires or the worker is canceled. The Paddle request has its own 20-second deadline, followed by a fresh 10-second deadline to attempt to record the outcome. If the write fails, the worker reports the error and the top-up can remain charging.
