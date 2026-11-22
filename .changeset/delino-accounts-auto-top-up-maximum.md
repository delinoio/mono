---
"delino-accounts": patch
---

Reject an enabled automatic top-up amount above $10,000 in the settings form with "Enter at most $10,000.00." before it sends a request, like the API server does. A disabled setting still keeps any whole-cent amount.
