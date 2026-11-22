---
"accounts-api": patch
---

Read the balance and the credit grants of `GetBalance` in one statement, so that a concurrent credit change cannot return the balance of one state with the grants of another.
