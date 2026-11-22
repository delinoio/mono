---
"delino-accounts": patch
---

Opening a billing account invitation link now deletes any invitation token that an earlier link left in `sessionStorage`, both on the first load and while the page is shown. Before, a later load without a fragment could accept the earlier invitation. A sign-in return and an address without a token keep the stored token.
