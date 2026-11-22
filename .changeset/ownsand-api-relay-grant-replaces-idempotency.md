---
"ownsand-api": patch
---

`AuthorizeRelay` now treats the requested `replaces_grant_id` as part of the request that an `authorization_id` identifies. Before, a retry with the same `authorization_id` that added, dropped or changed the predecessor returned the existing grant. Now it fails with `invalid_argument`, and an exact retry still returns the existing grant, also after the predecessor was released.
