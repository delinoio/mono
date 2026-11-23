---
"accounts-api": patch
---

Keep a newer `PAST_DUE` subscription status when the payment of an older billing period arrives late. The payment still adds its credit once. A payment of the stored period or a later one still makes the subscription `ACTIVE`.
