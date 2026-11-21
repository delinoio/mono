---
"accounts-api": minor
---

Add prepaid credits with Paddle as the Connect service `CreditService`: the required $10/month subscription through the Paddle checkout, manual top-ups of $25 to $10,000, automatic top-ups with a monthly cap and a shutoff after 2 failures, a credit ledger in micro-USD whose grants expire after 12 months and are consumed first-expiring first, refunds, low-balance, payment-failure and expiry emails, and invoice links. Credit is added only by verified, deduplicated Paddle webhooks on `POST /webhooks/paddle`. Deleting a billing account now cancels its Paddle subscription. The server needs the new `PADDLE_ENVIRONMENT`, `PADDLE_API_KEY`, `PADDLE_WEBHOOK_SECRET`, `PADDLE_SUBSCRIPTION_PRICE_ID` and `PADDLE_TOP_UP_PRODUCT_ID` configuration and serves Prometheus metrics on the optional internal `METRICS_ADDR` listener.
