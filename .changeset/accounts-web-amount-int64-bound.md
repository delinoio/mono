---
"delino-accounts": patch
---

The billing settings now reject an amount above $9,223,372,036,854.77 (the largest whole-cent value of the signed 64-bit API field) with the inline message `Enter at most $9,223,372,036,854.77.` before they send a request. This covers the alert threshold and the automatic top-up threshold, amount and monthly cap, also when automatic top-up is off. Before, such a value reached the server and showed a generic error. The $10,000 maximum of a top-up amount still applies.
