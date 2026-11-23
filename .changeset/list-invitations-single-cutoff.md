---
"ownsand-api": patch
---

`ListInvitations` now selects the invitations of a page and derives their state from the same time cutoff. An invitation that expires while the page is read stays out of the page or is reported as pending, where before it could be returned with the state `EXPIRED`.
