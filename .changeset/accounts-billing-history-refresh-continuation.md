---
"delino-accounts": patch
---

The "Show more usage", "Show older entries" and "Show older invoices" buttons no longer cancel a refresh of the list that is in flight or read on with the continuation token of the cached last page. The button stays disabled while the list refreshes, and after a failed refresh it stays disabled until "Try again" succeeds.
