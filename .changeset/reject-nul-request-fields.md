---
"ownsand-api": patch
---

Reject a U+0000 character in an `Enroll` device name or agent version, in a pinned agent version and in a sandbox label key or value with `invalid_argument` that names the field, instead of an internal error from the database. The check runs before the `Enroll` transaction, so the enrollment token stays usable. Other control characters stay valid.
