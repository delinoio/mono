---
"accounts-api": patch
---

Treat a null Logto user profile as a failed lookup. Billing notifications can retry delivery, and invitation recipients can retry acceptance with the same token.
