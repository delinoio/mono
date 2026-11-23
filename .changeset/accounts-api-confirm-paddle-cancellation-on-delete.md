---
"accounts-api": patch
---

Deleting a billing account now cancels the open first checkout transaction at Paddle, so a late payment cannot create a subscription for the deleted account. It also completes only when Paddle confirms the subscription cancellation: a 2xx answer without the canceled subscription fails the deletion with `unavailable` and changes nothing. After a rollback that follows a confirmed cancellation, the retry reads the canceled subscription at Paddle and completes the deletion with one forfeiture of the credit.
