---
"delino-accounts": patch
---

Opening `/callback` without a sign-in redirect in progress (a typed address, a bookmark or a reload after the sign-in finished) now leaves the route for `billing`, or for the pending invitation page, and replaces the history entry. Before, the page stayed on an empty `/callback`.
