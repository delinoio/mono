---
"delino-accounts": patch
---

A subscription start whose response arrives after the user left that billing account no longer opens the Paddle checkout. The page still reads its own account data again, and the checkout adapter returns without installing the Paddle event callback or opening the overlay when the request is stale, including while Paddle.js loads. A response for the account the user still views opens its checkout once.
