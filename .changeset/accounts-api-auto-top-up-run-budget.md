---
"accounts-api": patch
---

Limit each billing worker run to 30 seconds before it claims another queued automatic top-up, so that a long queue of slow Paddle charges cannot use the whole 2-minute tick and starve the later tasks (time-out of top-ups, grant expiry, suspensions, expiry notices, email delivery and metrics). A top-up that the run did not claim stays pending and is charged by a later run. A charge that has started is never canceled: it runs to its end and its result is recorded, so one charge may end after the 30 seconds.
