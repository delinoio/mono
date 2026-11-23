---
"accounts-api": patch
---

Bind a Paddle subscription to a billing account only through a recorded checkout transaction or a recorded subscription. The `billing_account_id` custom data of a webhook, which any browser checkout can set, now only adds credit: it no longer records or activates a subscription, and a failed renewal that matches only through it sends no email.
