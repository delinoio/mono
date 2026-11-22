---
"accounts-api": patch
---

Apply a credit grant at the smallest signed balance. The debt payoff no longer negates the balance, which overflowed at the minimum `int64` value, so the whole grant now pays off debt and stores no remaining credit.
