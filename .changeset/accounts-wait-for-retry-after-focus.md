---
"delino-accounts": patch
---

A complete list (billing accounts, members, invitations and linked organizations) whose later page failed now waits for "Try again" when the tab regains focus. Before, the focus refetch cleared the failure and requested the failed page again without the user's retry. A list without a failed later page still refetches on focus.
